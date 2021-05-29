package single_udp_tun

import (
	"encoding/binary"
	"fmt"
	"math/rand"
)

// u16		u16		u32
// size		rand	fletcher32
// -----------T---------------
//            |
//          fmix64
//            |
//          enc key            enc payload padding
// --------------------------- ----------- -------

const Obfs2HeaderSize = 8

type Obfs2 struct {
	*rand.Rand
}

// https://en.wikipedia.org/wiki/Fletcher's_checksum
// with modulo operations omitted
func fletcher32(payload []byte) uint32 {
	s1 := uint16(0)
	s2 := uint16(0)

	sz := len(payload) & (^1)
	for i := 0; i < sz; i += 2 {
		s1 += uint16(payload[i]) | (uint16(payload[i+1]) << 8)
		s2 += s1
	}
	if len(payload)&1 != 0 {
		s1 += uint16(payload[sz])
		s2 += s1
	}
	return (uint32(s2) << 16) | uint32(s1)
}

// from https://github.com/aappleby/smhasher/blob/master/src/MurmurHash3.cpp
func fmix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// from https://stackoverflow.com/a/9758173/3886899
func ximf64(h uint64) uint64 {
	h ^= h >> 33
	h *= 11291846944257947611
	h ^= h >> 33
	h *= 5725274745694666757
	h ^= h >> 33
	return h
}

func (self *Obfs2) Encode(header []byte, payload []byte) []byte {
	assert(len(payload) < (1 << 16))
	hdr := uint64(len(payload))
	hdr |= uint64(uint16(self.Rand.Uint32())) << 16
	hdr |= uint64(fletcher32(payload)) << 32
	hdr = fmix64(hdr)
	binary.LittleEndian.PutUint64(header, hdr)

	nPad := padLen(len(payload), self.Rand)
	assert(cap(header) >= 8+len(payload)+nPad) // TODO: grow buf
	header = header[:8+len(payload)+nPad]
	s := SplitMix64(hdr)
	s.XORKeyStream(header[8:], payload)
	s.XORKeyStream(header[8+len(payload):], header[8+len(payload):]) // random padding

	return header
}

func (self *Obfs2) Decode(dst []byte, src []byte) ([]byte, error) {
	if len(src) < 8 {
		return nil, fmt.Errorf("[len_src:%v] < 8", len(src))
	}

	hdr := binary.LittleEndian.Uint64(src[:8])
	s := SplitMix64(hdr)

	hdr = ximf64(hdr)
	sz := int(hdr & 0xffff)
	hdrsum := uint32(hdr >> 32)
	if len(src) < 8+sz {
		return nil, fmt.Errorf("[len_src:%v] < 8 + [sz:%v]", len(src), sz)
	}

	if sz > 0 {
		_ = dst[sz-1] // TODO: grow buf
		dst = dst[:sz]

		s.XORKeyStream(dst, src[8:8+sz])
	} else {
		dst = dst[:0]
	}
	if cksum := fletcher32(dst); cksum != hdrsum {
		return nil, fmt.Errorf("[cksum:%v] != [expected:%v]", cksum, hdrsum)
	}

	return dst, nil
}
