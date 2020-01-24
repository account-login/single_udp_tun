package main

import (
	"encoding/binary"
	"flag"
	"github.com/account-login/single_udp_tun"
	"github.com/pkg/errors"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"log"
	"math/rand"
	"net"
	"os"
	"sync/atomic"
	"unsafe"
)

const (
	// Stolen from https://godoc.org/golang.org/x/net/internal/iana,
	// can't import "internal" packages
	ProtocolICMP = 1
	//ProtocolIPv6ICMP = 58
)

type Client struct {
	Local      string
	Server     string
	ICMPID     int
	Src        int
	Dst        int
	Verbose    bool
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
	sconn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return errors.Wrap(err, "listen for server")
	}
	defer sconn.Close()
	log.Printf("[INFO]  listen on %v for server", sconn.LocalAddr())

	// server addr
	saddr, err := net.ResolveIPAddr("ip4", c.Server)
	if err != nil {
		return errors.Wrap(err, "resolve server addr")
	}
	log.Printf("[DEBUG] server addr: %v", saddr)

	// client addr
	var pcaddr unsafe.Pointer

	// local to server
	go func() {
		log.Printf("[DEBUG] ready to read from client")

		seqId := rand.Int()
		buf := make([]byte, 128*1024)
		// src dst
		binary.LittleEndian.PutUint32(buf[0:4], uint32(c.Src))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(c.Dst))

		for {
			// read from local
			n, addr, err := lconn.ReadFrom(buf[8:])
			if err != nil {
				log.Printf("[ERROR] read local: %v", err)
				continue
			}

			// store client addr
			oaddr := (*net.UDPAddr)(atomic.SwapPointer(&pcaddr, unsafe.Pointer(addr.(*net.UDPAddr))))
			if oaddr == nil {
				log.Printf("[INFO]  learned client addr: %v", addr)
			} else if !single_udp_tun.UDPAddrEqual(oaddr, addr.(*net.UDPAddr)) {
				log.Printf("[WARN]  client addr changed: %v -> %v", oaddr, addr)
			}

			// encode
			data := c.Obfuscator.Encode(buf[:n+8])

			// Make a new ICMP message
			seqId++
			m := icmp.Message{
				Type: ipv4.ICMPTypeEcho, Code: 0,
				Body: &icmp.Echo{ID: c.ICMPID, Seq: seqId, Data: data},
			}

			// write to server
			packed, err := m.Marshal(nil)
			if err != nil {
				panic(err)
			}
			_, err = sconn.WriteTo(packed, saddr)
			if err != nil {
				log.Printf("[ERROR] write server: %v", err)
				continue
			}
			if c.Verbose {
				log.Printf("[DEBUG] sent %v/%v data from %v to server %v",
					n, len(data), addr, saddr)
			}
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

			//// remote addr
			//if !addr.(*net.IPAddr).IP.Equal(saddr.IP) {
			//	log.Printf("[DEBUG] skip addr: %v", addr)
			//	continue
			//}

			// parse icmp reply
			// TODO: nocopy
			m, err := icmp.ParseMessage(ProtocolICMP, buf[:n])
			if err != nil {
				log.Printf("[ERROR] icmp.ParseMessage: %v", err)
				continue
			}
			if m.Type != ipv4.ICMPTypeEchoReply {
				log.Printf("[DEBUG] bad icmp type: %v", m.Type)
				continue
			}

			// decode
			data, err := c.Obfuscator.Decode(m.Body.(*icmp.Echo).Data)
			if err != nil {
				log.Printf("[ERROR] decode: %v", err)
				continue
			}

			// src, dst
			if len(data) < 8 {
				log.Printf("[ERROR] short packet, length: %v", len(data))
				continue
			}
			src := binary.LittleEndian.Uint32(data[0:4])
			dst := binary.LittleEndian.Uint32(data[4:8])
			data = data[8:]
			if src != uint32(c.Dst) || dst != uint32(c.Src) {
				log.Printf("[DEBUG] [src:%v][dst:%v] mismatch [datalen:%v]",
					uint32(c.Src), uint32(c.Dst), len(data))
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
			if c.Verbose {
				log.Printf("[DEBUG] write %v/%v data to client %v from server %v",
					len(data), n, caddr, addr)
			}
		}
	}()

	return nil
}

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	c := Client{}
	c.Obfuscator = single_udp_tun.NewRC4Obfs()

	flag.StringVar(&c.Local, "local", "0.0.0.0:5353", "listen for client")
	flag.StringVar(&c.Server, "server", "vm", "server addr")
	flag.IntVar(&c.ICMPID, "id", os.Getpid(), "icmp id")
	flag.IntVar(&c.Src, "src", 0, "src id")
	flag.IntVar(&c.Dst, "dst", 0, "dst id")
	flag.BoolVar(&c.Verbose, "verbose", false, "verbose log")
	flag.Parse()

	log.Fatal(c.Run())
}
