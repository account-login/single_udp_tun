package single_udp_tun

import "time"

// RDTSC gets tsc value out-of-order.
//go:noescape
func RDTSC() int64

type TSCCalibration struct {
	// the precision of this formula is limited by float64
	// unix = K * tsc + B
	K  float64
	B  float64
	R2 float64 // R^2
	N  int     // number of samples
	// improved fp precision
	// unix = K * (tsc - LastTSC) + LastUnix
	LastUnix int64
	LastTSC  int64
	// precise multiplication without fp
	Multiplier uint32
	Shift      uint32
}

var TSCParam TSCCalibration

// init TSCParam before use this
func TSCNano() int64 {
	return TSCParam.TSCNano()
}

func (cal *TSCCalibration) TSCNano() int64 {
	x := RDTSC()
	return cal.Cycle2Nano(x-cal.LastTSC) + cal.LastUnix
	//return int64(cal.K*float64(x-cal.LastTSC)) + cal.LastUnix
	//return int64(cal.K*float64(x) + cal.B)
}

func (cal *TSCCalibration) Cycle2Nano(v int64) int64 {
	return multiply(v, cal.Multiplier, cal.Shift)
}

func genMultiplier(K float64) (multiplier uint32, shift uint32) {
	assert(0 < K && K < 1) // FIXME: handle K >= 1
	for K < float64(1<<31) {
		K *= 2
		shift++
	}
	assert(K < float64(1<<32))
	assert(shift >= 32)
	multiplier = uint32(K)
	return
}

// NOTE: input uint64 and return uint64
func multiply(v int64, multiplier uint32, shift uint32) int64 {
	assert(shift >= 32)

	vh := uint64(v) >> 32
	vl := uint64(uint32(v))

	r := vh * uint64(multiplier) >> (shift - 32)
	r += vl * uint64(multiplier) >> (shift - 0)
	return int64(r)
}

func Calibrate(n int) (r TSCCalibration) {
	assert(n >= 2)
	r.N = n

	ibuf := make([]int64, 2*n)
	fbuf := make([]float64, 2*n)
	ox := ibuf[:n]
	oy := ibuf[n:]
	fx := fbuf[:n]
	fy := fbuf[n:]

	prev := time.Now().UnixNano()
	for i := 0; i < n; {
		unix := time.Now().UnixNano()
		tsc := RDTSC()
		if unix > prev { // for low resolution time.Now(), only accept data when unix ts is increasing
			ox[i] = tsc
			oy[i] = unix
			i++
		}
		prev = unix
	}

	// the (basex, basey) transforms samples into smaller numbers, this improves fp precision
	basex := ox[n/2]
	basey := oy[n/2]
	sx := 0.0
	sy := 0.0
	for i := 0; i < n; i++ {
		fx[i] = float64(ox[i] - basex)
		fy[i] = float64(oy[i] - basey)
		sx += fx[i]
		sy += fy[i]
	}

	// the least squares method
	meanx := sx / float64(n)
	meany := sy / float64(n)
	numer := 0.0
	denom := 0.0
	for i := 0; i < n; i++ {
		numer += (fx[i] - meanx) * (fy[i] - meany)
		denom += (fx[i] - meanx) * (fx[i] - meanx)
	}

	r.K = numer / denom
	r.B = meany - r.K*meanx

	sst := 0.0
	ssr := 0.0
	for i := 0; i < n; i++ {
		predy := r.B + r.K*fx[i]
		sst += (fy[i] - meany) * (fy[i] - meany)
		ssr += (fy[i] - predy) * (fy[i] - predy)
	}
	r.R2 = 1 - ssr/sst

	// used by TSCNano()
	r.LastTSC = basex
	r.LastUnix = basey + int64(r.B)
	r.Multiplier, r.Shift = genMultiplier(r.K)

	// revert basex, basey for `unix = K * tsc + B`
	r.B -= r.K * float64(basex)
	r.B += float64(basey)

	return
}
