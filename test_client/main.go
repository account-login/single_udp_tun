package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"time"
	"unsafe"
)

func assert(cond bool) {
	if !cond {
		panic("assertion fail")
	}
}

func main() {
	pSize := flag.Int("size", 0, "packet size")
	pPPS := flag.Int("pps", 0, "packet per second")
	pLoss := flag.Float64("loss", 0, "rate of loss")
	pDurtaion := flag.Int("duration", 0, "test duration in seconds")
	pServer := flag.String("server", "", "ip:port of server")
	pListen := flag.String("listen", "", "wait for connection from server")
	flag.Parse()

	size := *pSize
	if size < 8 {
		size = 1024
	}
	pps := *pPPS
	if pps <= 0 {
		pps = 1024
	}
	loss := *pLoss
	if loss <= 0 {
		loss = 0
	}
	duration := *pDurtaion
	if duration <= 0 {
		duration = 5
	}
	assert((*pServer == "") != (*pListen == "")) // one of

	rand.Seed(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(pSize))))

	var conn net.PacketConn
	var raddr net.Addr
	var err error
	if *pListen != "" {
		conn, err = net.ListenPacket("udp", *pListen)
		assert(err == nil)
		_, raddr, err = conn.ReadFrom(make([]byte, 1))
		assert(err == nil)
		fmt.Println("server:", raddr)
	} else {
		raddr, err = net.ResolveUDPAddr("udp", *pServer)
		assert(err == nil)
		conn, err = net.ListenPacket("udp", ":0")
		assert(err == nil)
	}

	pkt := make([]byte, size)

	total := pps * duration
	t0 := time.Now().UnixNano()
	np := 0
	nDrop := total
	for np < total {
		now := time.Now().UnixNano()
		ts := t0 + int64(float64(np+1)/float64(total)*float64(duration)*1e9)
		if ts > now {
			time.Sleep(time.Duration(ts-now) * time.Nanosecond)
		}

		if rand.Float64() > loss {
			binary.LittleEndian.PutUint64(pkt, uint64(np))
			_, err := conn.WriteTo(pkt, raddr)
			assert(err == nil)
			nDrop--
		}
		np++
	}

	realDuration := float64(time.Now().UnixNano()-t0) / 1e9
	fmt.Println("size:", size)
	fmt.Println("pps:", pps)
	fmt.Println("loss", float64(nDrop)/float64(total))
	fmt.Println("duration:", realDuration)
}
