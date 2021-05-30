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
	"math"
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
	Loss1  uint32
	Late1  uint32 // pkt arrived after timeout
	Count1 uint32
	Loss2  uint32
	Count2 uint32
	LossP  uint32 // the loss rate in percentage [0, 100]
}

type NodeExport struct {
	UnixMS int64
	Dst    string
	Stats  NodeStats
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
	return !(a-b < (1 << 63))
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

	const rttMax = int64(1 * 1000 * 1000 * 1000) // 1s
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
	rtovar := 4 * self.out.RTTVar
	if rtovar < self.out.SRTT/2 {
		rtovar = self.out.SRTT / 2
	}
	// NOTE: differs from rfc: RTO = SRTT + max(SRTT / 2, 4 * RTTVAR)
	// NOTE: more tolerent to rtt variance
	rto := self.out.SRTT + rtovar
	// 10ms <= rto <= 1s
	if rto > 1000*1000*1000 {
		rto = 1000 * 1000 * 1000
	}
	if rto < 10*1000*1000 {
		rto = 10 * 1000 * 1000
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
	self.out.Late1 = 0
	self.out.Loss2 = 0
	self.out.Count1 = 0
	self.out.Count2 = 0
	for seq := countEnd; !seqBefore(seq, countBegin); seq-- {
		ent := &self.ackPool[seq%kEntrySize]
		assert(ent.tsSend > 0 && ent.seq == seq)

		isLoss := uint32(0)
		isLate := uint32(0)
		if ent.tsRecv == 0 {
			isLoss = 1
		}
		if ent.tsRecv > ent.tsSend+rto {
			isLoss = 1
			isLate = 1
		}
		if self.out.Count1 < 100 {
			self.out.Loss1 += isLoss
			self.out.Late1 += isLate
			self.out.Count1++
		}
		if self.out.Count2 < 1000 {
			self.out.Loss2 += isLoss
			self.out.Count2++
		}
	}
	if self.out.Count1 > 10 {
		// this should over estimate most of the time
		lossRate := math.Max(
			float64(self.out.Loss1)/float64(self.out.Count1),
			float64(self.out.Loss2)/float64(self.out.Count2),
		)
		self.out.LossP = uint32(math.Ceil(lossRate * 100))
	}
}

func assert(condition bool) {
	if !condition {
		panic("assertion failure")
	}
}

type Probe struct {
	// params
	Bind    string
	Peers   []string
	SimLoss float64
	Obfs    bool
	// private
	conn      *net.UDPConn
	mu        sync.Mutex
	addr2node map[string]*Node
	randgen   *rand.Rand
}

func (self *Probe) addPeer(ctx context.Context, sess *dns.SessionUDP) *Node {
	addr := sess.RemoteAddr().String()
	node := &Node{
		seqNext:   self.randgen.Uint64(), // randomize the initial seq
		ackPrevTs: nanotime(),
	}

	if self.addr2node == nil {
		self.addr2node = map[string]*Node{}
	}
	self.addr2node[addr] = node

	// ping peer every 100ms
	go func() {
		ctx := ctxlog.Pushf(ctx, "[peer:%v]", addr)
		ctxlog.Infof(ctx, "started")

		obfs := single_udp_tun.Obfs2{} // uninitialized
		buf := make([]byte, 128*1024)

		for {
			pkt := buf[single_udp_tun.Obfs2HeaderSize:]
			pkt = pkt[:16]
			binary.LittleEndian.PutUint64(pkt[:8], kCmdReq)

			stop := func() bool {
				self.mu.Lock()
				defer self.mu.Unlock()

				// delete peer without ack in 10s
				if !node.isFixed && nanotime() > node.ackPrevTs+10*1000*1000*1000 {
					ctxlog.Warnf(ctx, "expired")
					delete(self.addr2node, addr)
					return true
				}

				// construct pkt and log the entry
				binary.LittleEndian.PutUint64(pkt[8:16], node.seqNext)
				node.ackPool[node.seqNext%kEntrySize] = Entry{
					seq:    node.seqNext,
					tsSend: nanotime(),
				}
				node.seqNext++

				// init obfs
				if self.Obfs && obfs.Rand == nil {
					obfs.Rand = rand.New(rand.NewSource(self.randgen.Int63()))
				}

				return false
			}()
			if stop {
				return
			}

			if self.Obfs {
				pkt = obfs.Encode(buf, pkt)
			}

			drop := self.SimLoss > 0 && self.randgen.Float64() < self.SimLoss
			if !drop {
				_, err := dns.WriteToSessionUDP(self.conn, pkt, sess)
				if err != nil {
					ctxlog.Errorf(ctx, "WriteToUDP() error: %v", err)
				}
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
	obfs := single_udp_tun.Obfs2{Rand: self.randgen}

	// create peers
	self.mu.Lock()
	for _, peer := range self.Peers {
		peerAddr, err := net.ResolveUDPAddr("udp", peer)
		if err != nil {
			return errors.Wrapf(err, "ResolveUDPAddr(%v)", peer)
		}
		node := self.addPeer(ctx, &dns.SessionUDP{RAddr: peerAddr, Context: nil})
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
		if self.Obfs {
			pkt, err = obfs.Decode(pkt[single_udp_tun.Obfs2HeaderSize:], pkt)
			if err != nil {
				ctxlog.Errorf(ctx, "obfs.Decode(): %v", err)
				continue
			}
		}
		//ctxlog.Debugf(ctx, "pkt: %v", pkt)

		cmd := binary.LittleEndian.Uint64(pkt[:8])
		if cmd == kCmdReq {
			pktReq := PktReq{}
			pktReq.seq = binary.LittleEndian.Uint64(pkt[8:16])

			self.mu.Lock()
			node := self.addr2node[raddr.String()]
			if node == nil {
				node = self.addPeer(ctx, sess)
			}
			pktAck := node.onReq(&pktReq)
			self.mu.Unlock()

			// reply
			pkt := buf[single_udp_tun.Obfs2HeaderSize:]
			pkt = pkt[:16+128]
			binary.LittleEndian.PutUint64(pkt[:8], kCmdAck)
			binary.LittleEndian.PutUint64(pkt[8:16], pktAck.seqEnd)
			copy(pkt[16:16+128], pktAck.bitmap[:])
			if self.Obfs {
				pkt = obfs.Encode(buf, pkt)
			}
			_, err := dns.WriteToSessionUDP(conn, pkt, sess) // FIXME: blocking
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
	nowMS := time.Now().UnixNano() / 1000000

	self.mu.Lock()
	defer self.mu.Unlock()

	for addr, node := range self.addr2node {
		export := NodeExport{
			UnixMS: nowMS,
			Dst:    addr,
			Stats:  node.out,
		}
		results = append(results, export)
	}
	return
}

func probePrint(p *Probe) {
	for {
		time.Sleep(1 * time.Second)
		for _, r := range p.Export() {
			buf, err := json.Marshal(&r)
			assert(err == nil)
			fmt.Println(string(buf))
		}
	}
}

// TODO: human ts
// TODO: node id
// TODO: kCmdExport
func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	p := Probe{}
	flag.StringVar(&p.Bind, "bind", ":200", "bind on address")
	peerList := ""
	flag.StringVar(&peerList, "peers", "", "comma seperated address for peers")
	flag.Float64Var(&p.SimLoss, "simloss", 0, "simulate packet loss rate for sending")
	flag.BoolVar(&p.Obfs, "obfs", false, "enable obfuscation")
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
