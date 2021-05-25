package single_udp_tun

import (
	"runtime"
	"unsafe"
)

// from https://en.wikipedia.org/wiki/Xorshift
type SplitMix64 uint64

func (s *SplitMix64) next() uint64 {
	result := uint64(*s)
	result += 0x9E3779B97f4A7C15
	*s = SplitMix64(result)
	result = (result ^ (result >> 30)) * 0xBF58476D1CE4E5B9
	result = (result ^ (result >> 27)) * 0x94D049BB133111EB
	return result ^ (result >> 31)
}

func (s *SplitMix64) unalignedXORKeyStream(dst, src []byte) {
	if len(src) == 0 {
		return
	}
	_ = dst[len(src)-1]

	//ulen := len(src) / 8
	//if ulen > 0 {
	//	s64 := *(*[]uint64)(unsafe.Pointer(&reflect.SliceHeader{
	//		Data: uintptr(unsafe.Pointer(&src[0])),
	//		Len:  ulen,
	//		Cap:  ulen,
	//	}))
	//	d64 := *(*[]uint64)(unsafe.Pointer(&reflect.SliceHeader{
	//		Data: uintptr(unsafe.Pointer(&dst[0])),
	//		Len:  ulen,
	//		Cap:  ulen,
	//	}))
	//	_ = d64[len(s64)-1]
	//	for i, su := range s64 {
	//		d64[i] = su ^ s.next()
	//	}
	//}

	wend := len(src) & (^7)
	for i := 0; i < wend; i += 8 {
		r := s.next()
		*(*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(&dst[0])) + uintptr(i))) =
			r ^ *(*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(&src[0])) + uintptr(i)))
	}

	// NOTE: not reusable
	if len(src)%8 > 0 {
		r := s.next()
		for bi := wend; bi < len(src); bi++ {
			dst[bi] = src[bi] ^ byte(r)
			r >>= 8
		}
	}
}

func (s *SplitMix64) genericXORKeyStream(dst, src []byte) {
	if len(src) == 0 {
		return
	}
	_ = dst[len(src)-1]

	for i := 0; i < len(src); i += 8 {
		r := s.next()
		for j := i; j < len(src) && j < i+8; j++ {
			dst[j] = src[j] ^ byte(r)
			r >>= 8
		}
	}
}

// unaligned and little endian
const kUALE = runtime.GOARCH == "386" || runtime.GOARCH == "amd64"

func aligned8(addr *byte) bool {
	return uintptr(unsafe.Pointer(addr))%8 == 0
}

func (s *SplitMix64) XORKeyStream(dst, src []byte) {
	if kUALE || (aligned8(&dst[0]) && aligned8(&src[0])) {
		s.unalignedXORKeyStream(dst, src)
	} else {
		s.genericXORKeyStream(dst, src)
	}
}
