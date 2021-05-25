package single_udp_tun

import (
	testassert "github.com/stretchr/testify/assert"
	"testing"
)

func TestFMix32(t *testing.T) {
	testassert.Equal(t, uint32(123), fmix32rev(fmix32(123)))
}
