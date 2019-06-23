package single_udp_tun

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestFMix32(t *testing.T) {
	assert.Equal(t, uint32(123), fmix32rev(fmix32(123)))
}
