package single_udp_tun

import (
	testassert "github.com/stretchr/testify/assert"
	"testing"
)

func TestFletcher32(t *testing.T) {
	testassert.Equal(t, uint32(0xf04ec729), fletcher32([]byte("abcde")))
	testassert.Equal(t, uint32(0x564e2d29), fletcher32([]byte("abcdef")))
	testassert.Equal(t, uint32(0x83df2d91), fletcher32([]byte("abcdefh")))
}
