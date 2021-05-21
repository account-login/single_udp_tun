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

//func getFD(conn *net.UDPConn) syscall.Handle {
//	if runtime.GOOS == "windows" {
//		/*
//		type conn struct {
//			fd *netFD
//		}
//		type fdMutex struct {
//			state uint64
//			rsema uint32
//			wsema uint32
//		}
//		type FD struct {
//			// Lock sysfd and serialize access to Read and Write methods.
//			fdmu fdMutex
//
//			// System file descriptor. Immutable until Close.
//			Sysfd syscall.Handle
//			...
//		*/
//
//		p := *(*uintptr)(unsafe.Pointer(conn)) // fd *netFD
//		return *(*syscall.Handle)(unsafe.Pointer(p + 16))
//	} else {
//		fp, err := conn.File()
//		assert(err == nil)
//		return syscall.Handle(fp.Fd())
//	}
//}

func main() {
	pSize := flag.Int("size", 0, "packet size")
	pPPS := flag.Int("pps", 0, "packet per second")
	pLoss := flag.Float64("loss", 0, "rate of loss")
	pDurtaion := flag.Int("duration", 0, "test duration in seconds")
	pServer := flag.String("server", "", "ip:port of server")
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

	rand.Seed(time.Now().UnixNano() ^ int64(uintptr(unsafe.Pointer(pSize))))

	conn, err := net.Dial("udp", *pServer)
	assert(err == nil)

	//{
	//	fd := getFD(conn.(*net.UDPConn))
	//	fmt.Println("fd:", fd)
	//	var buf [4]byte
	//	binary.LittleEndian.PutUint32(buf[:], 128*1024*1024)
	//	err = syscall.Setsockopt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, &buf[0], 4)
	//	assert(err == nil)
	//}

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
			_, err := conn.Write(pkt)
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
