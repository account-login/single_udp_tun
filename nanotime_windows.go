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

var nanoPerTick = int64(0)
var nanoOffset = int64(0)
var qpc *windows.Proc

func init() {
	k32 := windows.MustLoadDLL("kernel32.dll")
	qpf := k32.MustFindProc("QueryPerformanceFrequency")
	qpc = k32.MustFindProc("QueryPerformanceCounter")
	freq := int64(0)
	ok, _, _ := qpf.Call(uintptr(unsafe.Pointer(&freq)))
	assert(ok != 0)
	nanoPerTick = 1000 * 1000 * 1000 / freq // NOTE: loss of precision
	assert(nanoPerTick > 0)                 // 100 on my pc
	nanoOffset = time.Now().UnixNano() - nanotimeQPC()
}

func nanotimeQPC() int64 {
	cnt := int64(0)
	ok, _, _ := qpc.Call(uintptr(unsafe.Pointer(&cnt)))
	assert(ok != 0)
	return cnt*nanoPerTick + nanoOffset
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
