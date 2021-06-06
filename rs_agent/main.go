package main

import (
	"context"
	"encoding/binary"
	"flag"
	"github.com/account-login/ctxlog"
	"github.com/account-login/single_udp_tun"
	"github.com/pkg/errors"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync/atomic"
	"time"
	"unsafe"
)

type RSAgent struct {
	// addr
	ListenClient  string
	ListenPeer    string
	ConnectClient string
	ConnectPeer   string
	// rs
	Loss    float64
	FlushMs int
	Debug   bool
	SimLoss float64
	// obfs
	Obfs bool
	// loss_probe
	ProbePath string
}

func safeClose(ctx context.Context, closer io.Closer) {
	if err := closer.Close(); err != nil {
		ctxlog.Errorf(ctx, "close: %v", err)
	}
}

func (self *RSAgent) Run(ctx context.Context) error {
	if self.ConnectClient == "" && self.ConnectPeer == "" {
		return errors.New("-connect-client or -connect-peer must be supplied")
	}

	// parse addr
	var pcaddr unsafe.Pointer
	var ppaddr unsafe.Pointer
	if self.ConnectClient != "" {
		uaddr, err := net.ResolveUDPAddr("udp", self.ConnectClient)
		if err != nil {
			return errors.Wrapf(err, "parse client addr")
		}
		pcaddr = unsafe.Pointer(uaddr)
	}
	if self.ConnectPeer != "" {
		uaddr, err := net.ResolveUDPAddr("udp", self.ConnectPeer)
		if err != nil {
			return errors.Wrapf(err, "parse peer addr")
		}
		ppaddr = unsafe.Pointer(uaddr)
	}

	// listen
	cconn, err := net.ListenPacket("udp", self.ListenClient)
	if err != nil {
		return errors.Wrapf(err, "listen for client")
	}
	defer safeClose(ctx, cconn)
	pconn, err := net.ListenPacket("udp", self.ListenPeer)
	if err != nil {
		return errors.Wrapf(err, "listen for peer")
	}
	defer safeClose(ctx, pconn)

	// rand
	randgen := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(&self)))))

	// obfs
	obfs := single_udp_tun.Obfs2{Rand: randgen}
	obfsBuf := make([]byte, 128*1024)

	// loss_probe
	mon := single_udp_tun.LossMon{
		Path: self.ProbePath,
	}
	if self.ProbePath != "" {
		go mon.Run(ctx)
	}

	// from client to peer
	go func() {
		ctx := ctxlog.Push(ctx, "[c2p]")
		ctxlog.Debugf(ctx, "ready to read from client")

		output := func(ctx context.Context, shard []byte) {
			// read peer addr
			paddr := (*net.UDPAddr)(atomic.LoadPointer(&ppaddr))
			if paddr == nil {
				ctxlog.Errorf(ctx, "peer addr not learned")
				return
			}

			// simulate packet loss
			if self.SimLoss > 0 && randgen.Float64() < self.SimLoss {
				if self.Debug {
					ctxlog.Debugf(ctx, "simulate loss")
				}
				return
			}

			// obfs
			if self.Obfs {
				shard = obfs.Encode(obfsBuf, shard)
			}

			// write to peer
			_, err = pconn.WriteTo(shard, paddr)
			if err != nil {
				ctxlog.Errorf(ctx, "write peer: %v", err)
				return
			}

			if self.Debug {
				ctxlog.Debugf(ctx, "sent %v data to peer %v", len(shard), paddr)
			}
		}

		rsSender := single_udp_tun.RSSender{
			Output: output,
			Loss:   self.Loss,
			Debug:  self.Debug,
		}
		rsSender.SetGroupId(randgen.Uint32()) // hack to reduce group id collision after reboot

		updateLoss := func() {
			if self.ProbePath == "" {
				return
			}
			paddr := (*net.UDPAddr)(atomic.LoadPointer(&ppaddr))
			if paddr == nil {
				return
			}
			lossp, ok := mon.Get(paddr.IP)
			if !ok {
				return
			}
			rsSender.Loss = float64(lossp) / 100
			//ctxlog.Debugf(ctx, "updated loss: %v", rsSender.Loss)
		}

		nextFlushTs := time.Now().Add(time.Duration(self.FlushMs) * time.Millisecond)
		buf := make([]byte, 128*1024)
		for {
			// read from client
			_ = cconn.SetReadDeadline(nextFlushTs)
			n, addr, err := cconn.ReadFrom(buf[4:]) // reserve 4 bytes of length prefix

			// update loss before potentially inovoking rsSender
			updateLoss()

			// check timeout
			if err != nil {
				if !os.IsTimeout(err) {
					ctxlog.Errorf(ctx, "read client: %v", err)
				}
				goto LDone
			}

			if self.ConnectClient != "" {
				// verify client addr
				caddr := (*net.UDPAddr)(pcaddr)
				if !single_udp_tun.UDPAddrEqual(caddr, addr.(*net.UDPAddr)) {
					ctxlog.Debugf(ctx, "not client, drop: %v", addr)
					continue
				}
			} else {
				// learned client addr
				oaddr := (*net.UDPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
				if oaddr == nil {
					ctxlog.Infof(ctx, "learned client addr: %v", addr)
				} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
					ctxlog.Warnf(ctx, "client addr changed: %v -> %v", oaddr, addr)
				}
			}

			// 4 bytes of length prefix
			binary.LittleEndian.PutUint32(buf[0:4], uint32(n))

			// encode and send
			rsSender.Input(ctx, buf[:4+n])

		LDone:
			if now := time.Now(); !now.Before(nextFlushTs) {
				rsSender.Flush(ctx)
				nextFlushTs = now.Add(time.Duration(self.FlushMs) * time.Millisecond)
			}
		}
	}()

	// from peer to client
	func() {
		ctx := ctxlog.Push(ctx, "[p2c]")
		ctxlog.Debugf(ctx, "ready to read from peer")

		output := func(ctx context.Context, data []byte) {
			// read client addr
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				ctxlog.Errorf(ctx, "client addr not learned")
				return
			}

			// 4 bytes of length prefix
			if len(data) < 4 {
				ctxlog.Errorf(ctx, "len(data) < 4")
			}
			sz := binary.LittleEndian.Uint32(data[0:4])
			if len(data) < int(4+sz) {
				ctxlog.Errorf(ctx, "[len:%v] < 4 + [sz:%v]", len(data), sz)
				return
			}
			// actual data
			data = data[4 : 4+sz]

			// write to client
			if self.Debug {
				ctxlog.Debugf(ctx, "got %v data from peer", len(data))
			}
			_, err = cconn.WriteTo(data, caddr)
			if err != nil {
				ctxlog.Errorf(ctx, "client write: %v", err)
				return
			}
		}

		rsReceiver := single_udp_tun.RSReceiver{
			Output: output,
			Debug:  self.Debug,
		}
		buf := make([]byte, 128*1024)
		for {
			// read from peer
			n, addr, err := pconn.ReadFrom(buf)
			if err != nil {
				ctxlog.Errorf(ctx, "peer read: %v", err)
				continue
			}

			// deobfs
			pkt := buf[:n]
			if self.Obfs {
				pkt, err = obfs.Decode(pkt[single_udp_tun.Obfs2HeaderSize:], pkt)
				if err != nil {
					ctxlog.Errorf(ctx, "obfs.Decode(): %v", err)
					continue
				}
			}

			if self.ConnectPeer != "" {
				// verify peer addr
				paddr := (*net.UDPAddr)(ppaddr)
				if !single_udp_tun.UDPAddrEqual(paddr, addr.(*net.UDPAddr)) {
					ctxlog.Debugf(ctx, "not peer, drop: %v", addr)
					continue
				}
			} else {
				// store peer addr
				oaddr := (*net.UDPAddr)(atomic.SwapPointer(&ppaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
				if oaddr == nil {
					ctxlog.Infof(ctx, "learned peer addr: %v", addr)
				} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
					ctxlog.Warnf(ctx, "peer addr changed: %v -> %v", oaddr, addr)
				}
			}

			err = rsReceiver.Input(ctx, pkt)
			if err != nil {
				ctxlog.Errorf(ctx, "[RSReceiver::Input]: %v", err)
			}
		}
	}()

	return nil
}

// TODO: export rs stats
func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	p := RSAgent{}
	debugServer := ""

	flag.StringVar(&p.ListenClient, "listen-client", "127.0.0.1:700", "listen for client")
	flag.StringVar(&p.ListenPeer, "listen-peer", "0.0.0.0:800", "listen for peer")
	flag.StringVar(&p.ConnectClient, "connect-client", "", "connect to client")
	flag.StringVar(&p.ConnectPeer, "connect-peer", "", "connect to peer")

	flag.Float64Var(&p.Loss, "loss", 0, "loss rate")
	flag.IntVar(&p.FlushMs, "flush-ms", 20, "interval for flush RSSender (milliseconds)")
	flag.BoolVar(&p.Debug, "debug", false, "debug log")
	flag.Float64Var(&p.SimLoss, "simloss", 0, "simulate packet loss when sending packet to peer")

	flag.BoolVar(&p.Obfs, "obfs", false, "enable obfuscation")
	flag.StringVar(&p.ProbePath, "probe-path", "", "path to loss_probe output")
	flag.StringVar(&debugServer, "debug-server", "", "start a debug server at this address")
	flag.Parse()

	ctx := context.Background()
	if debugServer != "" {
		single_udp_tun.StartDebugServer(ctx, debugServer)
	}
	ctxlog.Fatal(ctx, p.Run(ctx))
}
