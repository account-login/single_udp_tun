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

type Server struct {
	// from client
	Addr string
	// to/from target
	Local string
	// target
	Target string
	// rs
	Loss    float64
	FlushMs int
	Debug   bool
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

func (s *Server) Run(ctx context.Context) error {
	// client
	cconn, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		return errors.Wrap(err, "listen for client")
	}
	defer safeClose(ctx, cconn)

	// target
	tconn, err := net.ListenPacket("udp", s.Local)
	if err != nil {
		return errors.Wrap(err, "listen for target")
	}
	defer safeClose(ctx, tconn)

	// target addr
	taddr, err := net.ResolveUDPAddr("udp", s.Target)
	if err != nil {
		return errors.Wrap(err, "resolve target")
	}
	ctxlog.Debugf(ctx, "target: %v", taddr)

	// client addr
	var pcaddr unsafe.Pointer

	// misc
	randgen := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(&s)))))

	// obfs
	obfs := single_udp_tun.Obfs2{Rand: randgen}
	obfsBuf := make([]byte, 128*1024)

	// loss_probe
	mon := single_udp_tun.LossMon{
		Path: s.ProbePath,
	}
	if s.ProbePath != "" {
		go mon.Run(ctx)
	}

	// target to client
	go func() {
		ctx := ctxlog.Push(ctx, "[t2c]")
		ctxlog.Debugf(ctx, "ready to read from target")

		output := func(ctx context.Context, shard []byte) {
			// read client addr
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				ctxlog.Errorf(ctx, "client addr not learned")
				return
			}

			// obfs
			if s.Obfs {
				shard = obfs.Encode(obfsBuf, shard)
			}

			// write to client
			_, err = cconn.WriteTo(shard, caddr)
			if err != nil {
				ctxlog.Errorf(ctx, "write client: %v", err)
				return
			}

			if s.Debug {
				ctxlog.Debugf(ctx, "sent %v data to client %v from target",
					len(shard), caddr)
			}
		}

		rsSender := single_udp_tun.RSSender{
			Output: output,
			Loss:   s.Loss,
			Debug:  s.Debug,
		}
		rsSender.SetGroupId(randgen.Uint32()) // hack to reduce group id collision after reboot

		updateLoss := func() {
			if s.ProbePath == "" {
				return
			}
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				return
			}
			lossp, ok := mon.Get(caddr.IP)
			if !ok {
				return
			}
			rsSender.Loss = float64(lossp) / 100
			//ctxlog.Debugf(ctx, "updated loss: %v", rsSender.Loss)
		}

		nextFlushTs := time.Now().Add(time.Duration(s.FlushMs) * time.Millisecond)
		buf := make([]byte, 128*1024)
		for {
			// read from target
			_ = tconn.SetReadDeadline(nextFlushTs)
			n, addr, err := tconn.ReadFrom(buf[4:]) // reserve 4 bytes of length prefix

			// update loss before potentially inovoking rsSender
			updateLoss()

			// check timeout
			if err != nil {
				if !os.IsTimeout(err) {
					ctxlog.Errorf(ctx, "read target: %v", err)
				}
				goto LDone
			}

			// verify target addr
			if !single_udp_tun.UDPAddrEqual(taddr, addr.(*net.UDPAddr)) {
				ctxlog.Debugf(ctx, "drop target: %v", addr)
				continue
			}

			// 4 bytes of length prefix
			binary.LittleEndian.PutUint32(buf[0:4], uint32(n))

			// encode and send
			rsSender.Input(ctx, buf[:4+n])

		LDone:
			if now := time.Now(); !now.Before(nextFlushTs) {
				rsSender.Flush(ctx)
				nextFlushTs = now.Add(time.Duration(s.FlushMs) * time.Millisecond)
			}
		}
	}()

	// client to target
	func() {
		ctx := ctxlog.Push(ctx, "[c2t]")
		ctxlog.Debugf(ctx, "ready to read from client")

		output := func(ctx context.Context, data []byte) {
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

			// write to target
			if s.Debug {
				ctxlog.Debugf(ctx, "got %v data from client", len(data))
			}
			_, err = tconn.WriteTo(data, taddr)
			if err != nil {
				ctxlog.Errorf(ctx, "target write: %v", err)
				return
			}
		}

		rsReceiver := single_udp_tun.RSReceiver{
			Output: output,
			Debug:  s.Debug,
		}
		buf := make([]byte, 128*1024)
		for {
			// read from client
			n, addr, err := cconn.ReadFrom(buf)
			if err != nil {
				ctxlog.Errorf(ctx, "client read: %v", err)
				continue
			}

			// deobfs
			pkt := buf[:n]
			if s.Obfs {
				pkt, err = obfs.Decode(pkt[single_udp_tun.Obfs2HeaderSize:], pkt)
				if err != nil {
					ctxlog.Errorf(ctx, "obfs.Decode(): %v", err)
					continue
				}
			}

			// store client addr
			oaddr := (*net.UDPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
			if oaddr == nil {
				ctxlog.Infof(ctx, "learned client addr: %v", addr)
			} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
				ctxlog.Warnf(ctx, "client addr changed: %v -> %v", oaddr, addr)
			}

			err = rsReceiver.Input(ctx, pkt)
			if err != nil {
				ctxlog.Errorf(ctx, "[RSReceiver::Input]: %v", err)
			}
		}
	}()

	return nil
}

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	s := Server{}
	debugServer := ""

	flag.StringVar(&s.Addr, "addr", "0.0.0.0:700", "listen for client")
	flag.StringVar(&s.Local, "local", "0.0.0.0:750", "listen for target")
	flag.StringVar(&s.Target, "target", "127.0.0.1:800", "target")
	flag.Float64Var(&s.Loss, "loss", 0, "loss rate")
	flag.IntVar(&s.FlushMs, "flush-ms", 20, "interval for flush RSSender (milliseconds)")
	flag.BoolVar(&s.Debug, "debug", false, "debug log")
	flag.BoolVar(&s.Obfs, "obfs", false, "enable obfuscation")
	flag.StringVar(&s.ProbePath, "probe-path", "", "path to loss_probe output")
	flag.StringVar(&debugServer, "debug-server", "", "start a debug server at this address")
	flag.Parse()

	ctx := context.Background()
	if debugServer != "" {
		single_udp_tun.StartDebugServer(ctx, debugServer)
	}
	ctxlog.Fatal(ctx, s.Run(ctx))
}
