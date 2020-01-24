package main

import (
	"encoding/binary"
	"flag"
	"github.com/account-login/single_udp_tun"
	"github.com/pkg/errors"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"log"
	"net"
	"sync/atomic"
	"unsafe"
)

const (
	// Stolen from https://godoc.org/golang.org/x/net/internal/iana,
	// can't import "internal" packages
	ProtocolICMP = 1
	//ProtocolIPv6ICMP = 58
)

type Server struct {
	//// from client
	//Addr string
	// to/from target
	Local string
	// target
	Target string
	// src dst
	Src     int
	Dst     int
	// log
	Verbose bool
	// obfs
	Obfuscator single_udp_tun.Obfuscator
}

type ICMPAddr struct {
	IPAddr *net.IPAddr
	ID     int
}

func (s *Server) Run() error {
	// client
	cconn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return errors.Wrap(err, "listen for client")
	}
	defer cconn.Close()

	// local
	lconn, err := net.ListenPacket("udp", s.Local)
	if err != nil {
		return errors.Wrap(err, "listen for target")
	}
	defer lconn.Close()

	// target addr
	taddr, err := net.ResolveUDPAddr("udp", s.Target)
	if err != nil {
		return errors.Wrap(err, "resolve target")
	}
	log.Printf("[DEBUG] target: %v", taddr)

	// client addr
	clientSeq := uint32(0)
	var pcaddr unsafe.Pointer // ICMPAddr

	// target to client
	go func() {
		log.Printf("[DEBUG] ready to read from target")

		buf := make([]byte, 128*1024)
		// src dst
		binary.LittleEndian.PutUint32(buf[0:4], uint32(s.Src))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(s.Dst))

		for {
			// read from target
			n, addr, err := lconn.ReadFrom(buf[8:])
			if err != nil {
				log.Printf("[ERROR] target read: %v", err)
				continue
			}

			// verify target addr
			if !single_udp_tun.UDPAddrEqual(taddr, addr.(*net.UDPAddr)) {
				log.Printf("[DEBUG] drop target: %v", addr)
				continue
			}

			// encode
			data := s.Obfuscator.Encode(buf[:8+n])

			// read client addr
			caddr := (*ICMPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				log.Printf("[ERROR] client addr not learned")
				continue
			}

			// Make a new ICMP message
			m := icmp.Message{
				Type: ipv4.ICMPTypeEchoReply, Code: 0,
				Body: &icmp.Echo{
					ID:   caddr.ID,
					Seq:  int(atomic.LoadUint32(&clientSeq)),
					Data: data,
				},
			}

			// write to client
			packed, err := m.Marshal(nil)
			if err != nil {
				panic(err)
			}
			_, err = cconn.WriteTo(packed, caddr.IPAddr)
			if err != nil {
				log.Printf("[ERROR] client write: %v", err)
				continue
			}
			if s.Verbose {
				log.Printf("[DEBUG] sent %v/%v data to client %v from target %v",
					n, len(data), caddr.IPAddr, addr)
			}
		}
	}()

	// client to target
	func() {
		log.Printf("[DEBUG] ready to read from client")

		buf := make([]byte, 128*1024)
		for {
			// read from client
			n, addr, err := cconn.ReadFrom(buf)
			if err != nil {
				log.Printf("[ERROR] client read: %v", err)
				continue
			}

			// parse icmp msg
			// TODO: nocopy
			m, err := icmp.ParseMessage(ProtocolICMP, buf[:n])
			if err != nil {
				log.Printf("[ERROR] icmp.ParseMessage: %v", err)
				continue
			}
			if _, ok := m.Body.(*icmp.Echo); !ok {
				log.Printf("[ERROR] bad icmp type: %v", m.Type)
				continue
			}

			// decode
			echo := m.Body.(*icmp.Echo)
			data, err := s.Obfuscator.Decode(echo.Data)
			if err != nil {
				log.Printf("[WARN]  client decode: %v", err)
				continue
			}

			// src dst
			if len(data) < 8 {
				log.Printf("[ERROR] short packet, length: %v", len(data))
				continue
			}
			src := binary.LittleEndian.Uint32(data[0:4])
			dst := binary.LittleEndian.Uint32(data[4:8])
			data = data[8:]
			if src != uint32(s.Dst) || dst != uint32(s.Src) {
				log.Printf("[DEBUG] [src:%v][dst:%v] mismatch [datalen:%v]",
					uint32(s.Src), uint32(s.Dst), len(data))
				continue
			}

			// store client addr
			atomic.StoreUint32(&clientSeq, uint32(echo.Seq))
			naddr := &ICMPAddr{
				IPAddr: addr.(*net.IPAddr),
				ID:     echo.ID,
			}
			oaddr := (*ICMPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(naddr)))
			if oaddr == nil {
				log.Printf("[INFO]  learned client addr: %v:%d", naddr.IPAddr, naddr.ID)
			} else if !(oaddr.IPAddr.IP.Equal(naddr.IPAddr.IP) && oaddr.ID == naddr.ID) {
				log.Printf("[WARN]  client addr changed: %v:%d -> %v:%d",
					oaddr.IPAddr, oaddr.ID, naddr.IPAddr, naddr.ID)
			}

			// write to target
			if s.Verbose {
				log.Printf("[DEBUG] got %v/%v data from client %v", len(data), n, addr)
			}
			n, err = lconn.WriteTo(data, taddr)
			if err != nil {
				log.Printf("[ERROR] target write: %v", err)
				continue
			}
		}
	}()

	return nil
}

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	s := Server{}
	s.Obfuscator = single_udp_tun.NewRC4Obfs()

	flag.StringVar(&s.Local, "local", "0.0.0.0:2121", "listen for target")
	flag.StringVar(&s.Target, "target", "8.8.8.8:53", "target")
	flag.IntVar(&s.Src, "src", 0, "src id")
	flag.IntVar(&s.Dst, "dst", 0, "dst id")
	flag.BoolVar(&s.Verbose, "verbose", false, "verbose log")
	flag.Parse()

	if s.Src == s.Dst || s.Src == 0 || s.Dst == 0 {
		log.Fatalf("bad [src:%v][dst:%v]", uint32(s.Src), uint32(s.Dst))
	}

	log.Fatal(s.Run())
}
