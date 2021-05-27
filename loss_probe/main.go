package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/account-login/ctxlog"
	"github.com/account-login/single_udp_tun"
	"github.com/account-login/single_udp_tun/dns"
	"github.com/pkg/errors"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const kEntrySize = 1024

type Entry struct {
	seq    uint64
	tsSend int64
	tsRecv int64
}

type Node struct {
	// sent req
	seqNext uint64
	ackPool [kEntrySize]Entry
	// received req
	reqPrev uint64
	reqPool [kEntrySize]uint64
	reqInit [kEntrySize]bool
	// for expiration
	isFixed   bool
	ackPrevTs int64
	// output
	out NodeStats
}

type NodeStats struct {
	SRTT   int64
	RTTVar int64
	Loss1  int64
	Count1 int64
	Loss2  int64
	Count2 int64
}

type NodeExport struct {
	Dst   string
	Stats NodeStats
}

type PktReq struct {
	seq uint64
}

type PktAck struct {
	seqEnd uint64
	bitmap [kEntrySize / 8]byte
}

// a < b
func seqBefore(a uint64, b uint64) bool {
	return b-a < (1<<63) && a != b
}

func (self *Node) onReq(pkt *PktReq) (ack *PktAck) {
	// save req
	diff := pkt.seq - self.reqPrev
	if diff > (1 << 63) {
		diff = ^uint64(0) - diff + 1
	}
	if diff >= kEntrySize {
		// the absolute difference is too large, reset states
		for i := range self.reqPool {
			self.reqPool[i] = 0
			self.reqInit[i] = false
		}
	}
	self.reqPool[pkt.seq%kEntrySize] = pkt.seq
	self.reqInit[pkt.seq%kEntrySize] = true
	self.reqPrev = pkt.seq

	// generate ack packet
	ack = &PktAck{}
	// ack.seqEnd is the maximum req seq so far
	ack.seqEnd = pkt.seq
	for i, req := range self.reqPool {
		if self.reqInit[i] && seqBefore(ack.seqEnd, req) {
			ack.seqEnd = req
		}
	}
	seqBegin := ack.seqEnd - kEntrySize + 1
	for i, req := range self.reqPool {
		if self.reqInit[i] && !seqBefore(req, seqBegin) && !seqBefore(ack.seqEnd, req) {
			idx := kEntrySize - (ack.seqEnd - req) - 1
			ack.bitmap[idx/8] |= byte(1) << (idx % 8)
		}
	}
	return
}

func (self *Node) onAck(pkt *PktAck) {
	now := nanotime()
	self.ackPrevTs = now

	rttMax := int64(1 * 1000 * 1000 * 1000) // 1s
	rtt := rttMax
	countBegin := pkt.seqEnd - kEntrySize + 1
	// for each seq from high to low
	for idx := uint64(kEntrySize - 1); idx < kEntrySize; idx-- {
		seq := pkt.seqEnd - kEntrySize + idx + 1

		ent := &self.ackPool[seq%kEntrySize]
		if ent.seq != seq || 0 == ent.tsSend {
			// ack on req with no record, stop
			countBegin = seq + 1
			break
		}

		// get rtt sample
		if 0 == (pkt.bitmap[idx/8] & (byte(1) << (idx % 8))) {
			continue
		}
		if ent.tsRecv != 0 {
			continue // already processed
		}
		ent.tsRecv = now
		diff := ent.tsRecv - ent.tsSend
		// NOTE: diff < 0 means the clock is screwed, diff == 0 may be caused by low resolution clock
		if diff >= 0 && diff < rtt {
			rtt = diff // NOTE: the smallest diff is the ack to the latest req
		}
	}

	// update rtt: https://datatracker.ietf.org/doc/html/rfc6298
	//    (2.2) When the first RTT measurement R is made, the host MUST set
	//
	//            SRTT <- R
	//            RTTVAR <- R/2
	//            RTO <- SRTT + max (G, K*RTTVAR)
	//
	//         where K = 4.
	//
	//   (2.3) When a subsequent RTT measurement R' is made, a host MUST set
	//
	//            RTTVAR <- (1 - beta) * RTTVAR + beta * |SRTT - R'|
	//            SRTT <- (1 - alpha) * SRTT + alpha * R'
	//
	//         The value of SRTT used in the update to RTTVAR is its value
	//         before updating SRTT itself using the second assignment.  That
	//         is, updating RTTVAR and SRTT MUST be computed in the above
	//         order.
	//
	//         The above SHOULD be computed using alpha=1/8 and beta=1/4 (as
	//         suggested in [JK88]).
	//
	//         After the computation, a host MUST update
	//         RTO <- SRTT + max (G, K*RTTVAR)
	if rtt < rttMax { // rtt too large is ignored
		if self.out.SRTT == 0 {
			self.out.SRTT = rtt
			self.out.RTTVar = rtt / 2
		} else {
			diff := self.out.SRTT - rtt
			if diff < 0 {
				diff = -diff
			}
			self.out.RTTVar -= self.out.RTTVar / 4
			self.out.RTTVar += diff / 4
			self.out.SRTT -= self.out.SRTT / 8
			self.out.SRTT += rtt / 8
		}
	}

	// count loss
	rto := self.out.SRTT + 4*self.out.RTTVar
	if rto > 1000*1000*1000 {
		rto = 1000 * 1000 * 1000 // max 1s
	}
	countEnd := pkt.seqEnd // the latest seq with rto ellapsed
	for self.ackPool[countEnd%kEntrySize].tsSend > now-rto && !seqBefore(countEnd, countBegin) {
		countEnd--
	}
	if seqBefore(countEnd, countBegin+10) {
		// not enough data
		return
	}

	self.out.Loss1 = 0
	self.out.Loss2 = 0
	self.out.Count1 = 0
	self.out.Count2 = 0
	for seq := countEnd; !seqBefore(seq, countBegin); seq-- {
		ent := &self.ackPool[seq%kEntrySize]
		assert(ent.tsSend > 0 && ent.seq == seq)

		isLoss := int64(0)
		if ent.tsRecv == 0 || ent.tsRecv > ent.tsSend+rto {
			isLoss = 1
		}
		if self.out.Count1 < 100 {
			self.out.Loss1 += isLoss
			self.out.Count1++
		}
		if self.out.Count2 < 1000 {
			self.out.Loss2 += isLoss
			self.out.Count2++
		}
	}
}

func assert(condition bool) {
	if !condition {
		panic("assertion failure")
	}
}

type Probe struct {
	// params
	Bind  string
	Peers []string
	// private
	conn      *net.UDPConn
	mu        sync.Mutex
	addr2node map[string]*Node
	randgen   *rand.Rand
}

func (self *Probe) addPeer(ctx context.Context, addr *net.UDPAddr) *Node {
	node := &Node{
		seqNext:   self.randgen.Uint64(), // randomize the initial seq
		ackPrevTs: nanotime(),
	}

	if self.addr2node == nil {
		self.addr2node = map[string]*Node{}
	}
	self.addr2node[addr.String()] = node

	// ping peer every 100ms
	go func() {
		ctx := ctxlog.Pushf(ctx, "[peer:%v]", addr)
		ctxlog.Infof(ctx, "start")

		buf := make([]byte, 8+8)
		binary.LittleEndian.PutUint64(buf[:8], kCmdReq)
		for {
			now := nanotime()

			self.mu.Lock()

			// peer no ack in 10s
			if !node.isFixed && nanotime() > node.ackPrevTs+10*1000*1000*1000 {
				ctxlog.Warnf(ctx, "expired")
				delete(self.addr2node, addr.String())
				self.mu.Unlock()
				return
			}

			binary.LittleEndian.PutUint64(buf[8:16], node.seqNext)
			node.ackPool[node.seqNext%kEntrySize] = Entry{
				seq:    node.seqNext,
				tsSend: now,
			}
			node.seqNext++

			self.mu.Unlock()

			_, err := self.conn.WriteToUDP(buf, addr)
			if err != nil {
				ctxlog.Errorf(ctx, "WriteToUDP() error: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	return node
}

// protocol
// cmd seq
//  8b  8b
// cmd seqEnd bitmap
//  8b     8b   128b

const (
	kCmdReq = 1
	kCmdAck = 2
)

func (self *Probe) Main(ctx context.Context) error {
	// bind and listen
	bindAddr, err := net.ResolveUDPAddr("udp", self.Bind)
	if err != nil {
		return errors.Wrapf(err, "ResolveUDPAddr(%v)", self.Bind)
	}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		return errors.Wrap(err, "ListenUDP()")
	}
	self.conn = conn
	_ = dns.SetSessionUDPOptions(conn)

	// create rng
	self.randgen = rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(&self)))))

	// create peers
	self.mu.Lock()
	for _, peer := range self.Peers {
		peerAddr, err := net.ResolveUDPAddr("udp", peer)
		if err != nil {
			return errors.Wrapf(err, "ResolveUDPAddr(%v)", peer)
		}
		node := self.addPeer(ctx, peerAddr)
		node.isFixed = true
	}
	self.mu.Unlock()

	// loop for reading incomming packets
	buf := make([]byte, 128*1024)
	for {
		n, sess, err := dns.ReadFromSessionUDP(conn, buf)
		if err != nil {
			ctxlog.Errorf(ctx, "ReadFromUDP(): %v", err)
			continue
		}

		raddr := sess.RemoteAddr().(*net.UDPAddr)
		pkt := buf[:n]
		if len(pkt) < 16 {
			ctxlog.Warnf(ctx, "bad pkt [from:%s]: %s", raddr, pkt)
			continue
		}
		//ctxlog.Debugf(ctx, "pkt: %v", pkt)

		cmd := binary.LittleEndian.Uint64(pkt[:8])
		if cmd == kCmdReq {
			pktReq := PktReq{}
			pktReq.seq = binary.LittleEndian.Uint64(pkt[8:16])

			self.mu.Lock()
			node := self.addr2node[raddr.String()]
			if node == nil {
				node = self.addPeer(ctx, raddr)
			}
			pktAck := node.onReq(&pktReq)
			self.mu.Unlock()

			// reply
			binary.LittleEndian.PutUint64(buf[:8], kCmdAck)
			binary.LittleEndian.PutUint64(buf[8:16], pktAck.seqEnd)
			copy(buf[16:16+128], pktAck.bitmap[:])
			_, err := dns.WriteToSessionUDP(conn, buf[:16+128], sess)
			if err != nil {
				ctxlog.Errorf(ctx, "reply [peer:%s] error: %v", raddr, err)
				continue
			}
		} else if cmd == kCmdAck {
			if len(pkt) < 8+8+128 {
				ctxlog.Warnf(ctx, "bad pkt [from:%s]: %s", raddr, pkt)
				continue
			}
			pktAck := PktAck{}
			pktAck.seqEnd = binary.LittleEndian.Uint64(pkt[8:16])
			copy(pktAck.bitmap[:], pkt[16:16+128])

			self.mu.Lock()
			node := self.addr2node[raddr.String()]
			if node != nil {
				node.onAck(&pktAck)
			} else {
				ctxlog.Warnf(ctx, "ack [from:%s] unknown", raddr)
			}
			self.mu.Unlock()
		} else {
			ctxlog.Warnf(ctx, "bad pkt [from:%s]: %s", raddr, pkt)
			continue
		}
	} // incomming packets
}

func nanotime() int64 {
	return single_udp_tun.Nanotime()
}

func (self *Probe) Export() (results []NodeExport) {
	self.mu.Lock()
	defer self.mu.Unlock()

	for addr, node := range self.addr2node {
		export := NodeExport{
			Dst:   addr,
			Stats: node.out,
		}
		results = append(results, export)
	}
	return
}

func probePrint(p *Probe) {
	for {
		time.Sleep(1 * time.Second)
		results := p.Export()
		for _, r := range results {
			buf, err := json.Marshal(&r)
			assert(err == nil)
			fmt.Println(string(buf))
		}
	}
}

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	//t := nanotime()
	//for {
	//	t2 := nanotime()
	//	if t2 != t {
	//		println(t2 - t)
	//		t = t2
	//	}
	//}

	p := Probe{}
	flag.StringVar(&p.Bind, "bind", ":200", "bind on address")
	peerList := ""
	flag.StringVar(&peerList, "peers", "", "comma seperated address for peers")
	flag.Parse()
	if peerList != "" {
		p.Peers = strings.Split(peerList, ",")
	}

	go probePrint(&p)
	ctx := context.Background()
	err := p.Main(ctx)
	if err != nil {
		ctxlog.Fatal(ctx, err)
	}
}
