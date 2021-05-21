package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/bits"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"time"
	"unsafe"
)

func assert(cond bool) {
	if !cond {
		panic("assertion fail")
	}
}

func main() {
	pNum := flag.Int("num", 0, "expected number of packets")
	pListen := flag.String("listen", "", "listen at this address")
	flag.Parse()

	rand.Seed(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(pNum))))

	num := *pNum
	assert(num > 0)
	udpAddr, err := net.ResolveUDPAddr("udp", *pListen)
	assert(err == nil)
	conn, err := net.ListenUDP("udp", udpAddr)
	assert(err == nil)

	bitmap := make([]uint64, num/64+1)
	buf := make([]byte, 64*1024)
	dup := 0
	go func() {
		for {
			n, _, err := conn.ReadFrom(buf)
			assert(err == nil)
			assert(n >= 8)

			seq := int(binary.LittleEndian.Uint64(buf))
			assert(seq < num)
			if (bitmap[seq/64] & (uint64(1) << (uint64(seq) % 64))) != 0 {
				dup++
			}
			bitmap[seq/64] |= uint64(1) << (uint64(seq) % 64)
		}
	}()

	// wait for ctrl-C
	sigc := make(chan os.Signal)
	signal.Notify(sigc, os.Interrupt)
	<-sigc

	defer func() {
		got := 0
		for _, m := range bitmap {
			got += bits.OnesCount64(m)
		}
		fmt.Println("loss:", float64(num-got)/float64(num))
		fmt.Println("dup:", dup)
	}()
}
