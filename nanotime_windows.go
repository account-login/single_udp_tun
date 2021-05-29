// +build windows

package single_udp_tun

import (
	"golang.org/x/sys/windows"
	"time"
	"unsafe"
)

func Nanotime() int64 {
	return nanotimeQPC()
}

var qpcMulti = uint32(0)
var qpcShift = uint32(0)
var qpcOffset = int64(0)
var qpcFn *windows.Proc

func init() {
	k32 := windows.MustLoadDLL("kernel32.dll")
	qpf := k32.MustFindProc("QueryPerformanceFrequency")
	qpcFn = k32.MustFindProc("QueryPerformanceCounter")
	freq := int64(0)
	ok, _, _ := qpf.Call(uintptr(unsafe.Pointer(&freq)))
	assert(ok != 0)

	nanoPerCycle := 1000 * 1000 * 1000 / float64(freq)
	for nanoPerCycle < float64(1<<31) {
		nanoPerCycle *= 2
		qpcShift++
	}
	qpcMulti = uint32(nanoPerCycle)
	assert(qpcShift < 32)

	qpcOffset = time.Now().UnixNano() - nanotimeQPC()
}

func nanotimeQPC() int64 {
	cnt := int64(0)
	ok, _, _ := qpcFn.Call(uintptr(unsafe.Pointer(&cnt)))
	assert(ok != 0)

	vh := uint64(cnt) >> 32
	vl := uint64(uint32(cnt))
	nanos := (vh * uint64(qpcMulti)) << (32 - qpcShift)
	nanos += (vl * uint64(qpcMulti)) >> qpcShift
	return int64(nanos) + qpcOffset
}

//// KUSER_SHARED_DATA, 1ms resolution
//func nanotimeInt() int64 {
//	for {
//		hilo := *(*int64)(unsafe.Pointer(uintptr(0x7ffe0008)))
//		hi2 := *(*int32)(unsafe.Pointer(uintptr(0x7ffe0008 + 8)))
//		if hi2 == int32(hilo>>32) {
//			return hilo * 100
//		}
//	}
//}
