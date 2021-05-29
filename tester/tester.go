package main

import (
	"fmt"
	"github.com/account-login/single_udp_tun"
	"time"
)

func nanotime() int64 {
	//return single_udp_tun.TSCNano()
	return single_udp_tun.Nanotime()
}

func main() {
	single_udp_tun.TSCParam = single_udp_tun.Calibrate(1000)
	fmt.Println(single_udp_tun.TSCParam)

	for i := 0; true; i++ {
		time.Sleep(time.Second)
		println(i, time.Now().UnixNano()-nanotime())
	}
}
