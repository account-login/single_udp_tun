package single_udp_tun

import (
	"crypto/rc4"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"time"
)

type Obfuscator interface {
	Encode([]byte) []byte
	Decode([]byte) ([]byte, error)
}

func UDPAddrEqual(lhs, rhs *net.UDPAddr) bool {
	return lhs.IP.Equal(rhs.IP) && lhs.Zone == rhs.Zone && lhs.Port == rhs.Port
}

type NilObfs struct{}

func (NilObfs) Encode(in []byte) []byte {
	return in
}

func (NilObfs) Decode(in []byte) ([]byte, error) {
	return in, nil
}

type RC4Obfs struct {
	*rand.Rand
}

func padLen(origin int, rand *rand.Rand) int {
	const padLimit = 1000
	if origin >= padLimit {
		return 0
	}
	return rand.Intn(padLimit - origin)
}

func NewRC4Obfs() *RC4Obfs {
	return &RC4Obfs{rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// from https://github.com/aappleby/smhasher/blob/master/src/MurmurHash3.cpp
func fmix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}

// from https://stackoverflow.com/a/9758173/3886899
func fmix32rev(h uint32) uint32 {
	h ^= h >> 16
	h *= 2127672349
	h ^= h >> 13
	h ^= h >> 26
	h *= 2781581891
	h ^= h >> 16
	return h
}

func (o RC4Obfs) Encode(in []byte) []byte {
	pad := padLen(len(in), o.Rand)
	buf := make([]byte, 4+len(in)+pad)

	// random + padding length
	r := uint32(o.Rand.Intn(0x10000)) << 16
	r |= uint32(pad)
	binary.LittleEndian.PutUint32(buf[:4], fmix32(r))

	// body
	cipher, _ := rc4.NewCipher(buf[:4])
	cipher.XORKeyStream(buf[4:], in)

	// padding
	o.Rand.Read(buf[4+len(in):])

	return buf
}

func (RC4Obfs) Decode(in []byte) ([]byte, error) {
	if len(in) < 4 {
		return nil, fmt.Errorf("packet length %v < 4", len(in))
	}

	pad := fmix32rev(binary.LittleEndian.Uint32(in[:4])) & 0xffff
	bodyLen := len(in) - 4 - int(pad)
	if bodyLen < 0 {
		return nil, fmt.Errorf("bad packet: packet length:%v padding length:%v",
			len(in), pad)
	}

	cipher, _ := rc4.NewCipher(in[:4])
	cipher.XORKeyStream(in[4:], in[4:4+bodyLen])

	return in[4 : 4+bodyLen], nil
}
