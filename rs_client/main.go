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

type Client struct {
	Local     string
	Server    string
	Loss      float64
	FlushMs   int
	Debug     bool
	SimLoss   float64
	Obfs      bool
	ProbePath string
}

func safeClose(ctx context.Context, closer io.Closer) {
	if err := closer.Close(); err != nil {
		ctxlog.Errorf(ctx, "close: %v", err)
	}
}

func getRandomUDPConn() (net.PacketConn, error) {
	var conns []net.PacketConn
	defer func() {
		for _, conn := range conns {
			if conn != nil {
				safeClose(context.Background(), conn)
			}
		}
	}()

	for i := 0; i < 111; i++ {
		conn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			return nil, errors.Wrap(err, "listen for server")
		}
		conns = append(conns, conn)
	}

	idx := int(time.Now().UnixNano()/1000000) % len(conns)
	conn := conns[idx]
	conns[idx] = nil
	return conn, nil
}

func (c *Client) Run(ctx context.Context) error {
	// local
	lconn, err := net.ListenPacket("udp", c.Local)
	if err != nil {
		return errors.Wrap(err, "listen on local")
	}
	defer safeClose(ctx, lconn)

	// server
	sconn, err := getRandomUDPConn()
	if err != nil {
		return errors.Wrap(err, "listen for server")
	}
	defer safeClose(ctx, sconn)
	ctxlog.Infof(ctx, "listen on %v for server", sconn.LocalAddr())

	// server addr
	saddr, err := net.ResolveUDPAddr("udp", c.Server)
	if err != nil {
		return errors.Wrap(err, "resolve server addr")
	}
	ctxlog.Debugf(ctx, "server addr: %v", saddr)

	// client addr
	var pcaddr unsafe.Pointer

	// misc
	randgen := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(&c)))))

	// obfs
	obfs := single_udp_tun.Obfs2{Rand: randgen}
	obfsBuf := make([]byte, 128*1024)

	// loss_probe
	mon := single_udp_tun.LossMon{
		Path: c.ProbePath,
	}
	if c.ProbePath != "" {
		go mon.Run(ctx)
	}

	// local to server
	go func() {
		ctx := ctxlog.Push(ctx, "[l2s]")
		ctxlog.Debugf(ctx, "ready to read from client")

		output := func(ctx context.Context, shard []byte) {
			// simulate packet loss
			if c.SimLoss > 0 && randgen.Float64() < c.SimLoss {
				if c.Debug {
					ctxlog.Debugf(ctx, "simulate loss")
				}
				return
			}

			// obfs
			if c.Obfs {
				shard = obfs.Encode(obfsBuf, shard)
			}

			// write to server
			_, err = sconn.WriteTo(shard, saddr)
			if err != nil {
				ctxlog.Errorf(ctx, "write server: %v", err)
				return
			}
			if c.Debug {
				ctxlog.Debugf(ctx, "sent %v bytes from %v to server %v",
					len(shard), "client", saddr)
			}
		}

		rsSender := single_udp_tun.RSSender{
			Output: output,
			Loss:   c.Loss,
			Debug:  c.Debug,
		}
		rsSender.SetGroupId(randgen.Uint32()) // hack to reduce group id collision after reboot

		updateLoss := func() {
			if c.ProbePath == "" {
				return
			}
			lossp, ok := mon.Get(saddr.IP)
			if !ok {
				return
			}
			rsSender.Loss = float64(lossp) / 100
			//ctxlog.Debugf(ctx, "updated loss: %v", rsSender.Loss)
		}

		nextFlushTs := time.Now().Add(time.Duration(c.FlushMs) * time.Millisecond)
		buf := make([]byte, 128*1024)
		for {
			// read from local
			_ = lconn.SetReadDeadline(nextFlushTs)
			n, addr, err := lconn.ReadFrom(buf[4:]) // reserve 4 bytes of length prefix

			// update loss before potentially inovoking rsSender
			updateLoss()

			if err != nil {
				if !os.IsTimeout(err) {
					ctxlog.Errorf(ctx, "read local: %v", err)
				}
				goto LDone
			}

			{
				// store client addr
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
				nextFlushTs = now.Add(time.Duration(c.FlushMs) * time.Millisecond)
			}
		}
	}()

	// server to local
	func() {
		ctx := ctxlog.Push(ctx, "[s2l]")
		ctxlog.Debugf(ctx, "ready to read from server")

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

			// read client addr
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				ctxlog.Errorf(ctx, "client addr not learned")
				return
			}

			// write to client
			_, err = lconn.WriteTo(data, caddr)
			if err != nil {
				ctxlog.Errorf(ctx, "write server: %v", err)
				return
			}
			if c.Debug {
				ctxlog.Debugf(ctx, "write %v data to client %v from server", len(data), caddr)
			}
		}

		rsReceiver := single_udp_tun.RSReceiver{
			Output: output,
			Debug:  c.Debug,
		}
		buf := make([]byte, 128*1024)
		for {
			// read from server
			n, _, err := sconn.ReadFrom(buf)
			if err != nil {
				ctxlog.Errorf(ctx, "read server: %v", err)
				continue
			}

			// deobfs
			pkt := buf[:n]
			if c.Obfs {
				pkt, err = obfs.Decode(pkt[single_udp_tun.Obfs2HeaderSize:], pkt)
				if err != nil {
					ctxlog.Errorf(ctx, "obfs.Decode(): %v", err)
					continue
				}
			}

			err = rsReceiver.Input(ctx, pkt)
			if err != nil {
				ctxlog.Errorf(ctx, "[RSReceiver::Input]: %v", err)
			}
		}
	}()

	return nil // unreachable
}

// TODO: export rs stats
func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	c := Client{}
	debugServer := ""

	flag.StringVar(&c.Local, "local", "0.0.0.0:600", "listen for client")
	flag.StringVar(&c.Server, "server", "127.0.0.1:700", "server addr")
	flag.Float64Var(&c.Loss, "loss", 0, "loss rate")
	flag.IntVar(&c.FlushMs, "flush-ms", 20, "interval for flush RSSender (milliseconds)")
	flag.BoolVar(&c.Debug, "debug", false, "debug log")
	flag.Float64Var(&c.SimLoss, "simloss", 0, "simulate sending packet loss")
	flag.BoolVar(&c.Obfs, "obfs", false, "enable obfuscation")
	flag.StringVar(&c.ProbePath, "probe-path", "", "path to loss_probe output")
	flag.StringVar(&debugServer, "debug-server", "", "start a debug server at this address")
	flag.Parse()

	ctx := context.Background()
	if debugServer != "" {
		single_udp_tun.StartDebugServer(ctx, debugServer)
	}
	ctxlog.Fatal(ctx, c.Run(ctx))
}
