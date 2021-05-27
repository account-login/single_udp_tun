// +build !windows

package single_udp_tun

import "time"

var t0 = time.Now()

func Nanotime() int64 {
	return time.Now().Sub(t0).Nanoseconds()
}
