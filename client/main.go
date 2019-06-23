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

type Client struct {
	Local      string
	Server     string
	Obfuscator single_udp_tun.Obfuscator
}

func (c *Client) Run() error {
	// local
	lconn, err := net.ListenPacket("udp", c.Local)
	if err != nil {
		return errors.Wrap(err, "listen on local")
	}
	defer lconn.Close()

	// server
	sconn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return errors.Wrap(err, "listen for server")
	}
	defer sconn.Close()
	log.Printf("[INFO] listen on %v for server", sconn.LocalAddr())

	// server addr
	saddr, err := net.ResolveUDPAddr("udp", c.Server)
	if err != nil {
		return errors.Wrap(err, "resolve server addr")
	}
	log.Printf("[DEBUG] server addr: %v", saddr)

	// client addr
	var pcaddr unsafe.Pointer

	// local to server
	go func() {
		log.Printf("[DEBUG] ready to read from client")

		buf := make([]byte, 128*1024)
		for {
			// read from local
			n, addr, err := lconn.ReadFrom(buf)
			if err != nil {
				log.Printf("[ERROR] read local: %v", err)
				continue
			}

			// store client addr
			oaddr := (*net.UDPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
			if oaddr == nil {
				log.Printf("[INFO] learned client addr: %v", addr)
			} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
				log.Printf("[WARN] client addr changed: %v -> %v", oaddr, addr)
			}

			// encode
			data := c.Obfuscator.Encode(buf[:n])

			// write to server
			_, err = sconn.WriteTo(data, saddr)
			if err != nil {
				log.Printf("[ERROR] write server: %v", err)
				continue
			}
			log.Printf("[DEBUG] sent %v/%v data from %v to server %v",
				n, len(data), addr, saddr)
		}
	}()

	// server to local
	func() {
		log.Printf("[DEBUG] ready to read from server")

		buf := make([]byte, 128*1024)
		for {
			// read from server
			n, addr, err := sconn.ReadFrom(buf)
			if err != nil {
				log.Printf("[ERROR] read server: %v", err)
				continue
			}

			// decode
			data, err := c.Obfuscator.Decode(buf[:n])
			if err != nil {
				log.Printf("[ERROR] decode: %v", err)
				continue
			}

			// read client addr
			caddr := (*net.UDPAddr)(atomic.LoadPointer(&pcaddr))
			if caddr == nil {
				log.Printf("[ERROR] client addr not learned")
				continue
			}

			// write to client
			_, err = lconn.WriteTo(data, caddr)
			if err != nil {
				log.Printf("[ERROR] write server: %v", err)
				continue
			}
			log.Printf("[DEBUG] write %v/%v data to client %v from server %v",
				len(data), n, caddr, addr)
		}
	}()

	return nil
}

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	c := Client{}
	c.Obfuscator = single_udp_tun.NewRC4Obfs()

	flag.StringVar(&c.Local, "local", "0.0.0.0:5353", "listen for client")
	flag.StringVar(&c.Server, "server", "do-sfo2:21", "server addr")
	flag.Parse()

	log.Fatal(c.Run())
}
