package main

import (
	"flag"
	"github.com/account-login/single_udp_tun"
	"github.com/pkg/errors"
	"log"
	"net"
	"sync/atomic"
	"unsafe"
)

type Server struct {
	// from client
	Addr string
	// to/from target
	Local string
	// target
	Target string
	// obfs
	Obfuscator single_udp_tun.Obfuscator
}

func (s *Server) Run() error {
	// client
	cconn, err := net.ListenPacket("udp", s.Addr)
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
	var pcaddr unsafe.Pointer

	// target to client
	go func() {
		log.Printf("[DEBUG] ready to read from target")

		buf := make([]byte, 128*1024)
		for {
			// read from target
			n, addr, err := lconn.ReadFrom(buf)
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
			data := s.Obfuscator.Encode(buf[:n])

			// read client addr
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				log.Printf("[ERROR] client addr not learned")
				continue
			}

			// write to client
			_, err = cconn.WriteTo(data, caddr)
			if err != nil {
				log.Printf("[ERROR] client write: %v", err)
				continue
			}
			log.Printf("[DEBUG] sent %v/%v data to client %v from target %v",
				n, len(data), caddr, addr)
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

			// store client addr
			oaddr := (*net.UDPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
			if oaddr == nil {
				log.Printf("[INFO] learned client addr: %v", addr)
			} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
				log.Printf("[WARN] client addr changed: %v -> %v", oaddr, addr)
			}

			// decode
			data, err := s.Obfuscator.Decode(buf[:n])
			if err != nil {
				log.Printf("[WARN] client decode: %v", err)
				continue
			}

			// write to target
			log.Printf("[DEBUG] got %v/%v data from client %v", len(data), n, addr)
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

	flag.StringVar(&s.Addr, "addr", "0.0.0.0:21", "listen for client")
	flag.StringVar(&s.Local, "local", "0.0.0.0:2121", "listen for target")
	flag.StringVar(&s.Target, "target", "8.8.8.8:53", "target")
	flag.Parse()

	log.Fatal(s.Run())
}
