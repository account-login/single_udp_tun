package single_udp_tun

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/account-login/ctxlog"
	"github.com/klauspost/reedsolomon"
	"math"
)

/*
protocol:
4B  group_id
1B  idx
1B  n_data
1B	n_parity
1B  reserved
var payload
*/

type RSSender struct {
	// params
	Output func(context.Context, []byte)
	Loss   float64
	Debug  bool
	// output
	Stats RSSenderStats
	// private
	shards   [][]byte
	groupId  uint32
	encoders map[uint16]reedsolomon.Encoder
}

type RSSenderStats struct {
	totalGroups       int64
	totalShards       int64
	totalBytes        int64
	totalParityShards int64
	totalParityBytes  int64
}

// the initial group id
// must not use fixed initial group id, to prevent collision after sender reboot
func (self *RSSender) SetGroupId(groupId uint32) {
	self.groupId = groupId
}

const kRSSenderMaxDataShard = 40

func (self *RSSender) Input(ctx context.Context, data []byte) {
	if self.Debug {
		ctxlog.Debugf(ctx, "[RSSender::Input] [group:%v][idx:%v] [size:%v]",
			self.groupId, len(self.shards), len(data))
	}

	encoded := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(encoded[0:4], self.groupId)
	encoded[4] = byte(len(self.shards)) // idx
	encoded[5] = 0                      // n_data
	encoded[6] = 0                      // n_parity
	encoded[7] = 0                      // reserved
	copy(encoded[8:], data)
	self.Output(ctx, encoded)
	self.Stats.totalShards++
	self.Stats.totalBytes += int64(len(data)) // no header

	self.shards = append(self.shards, encoded[8:]) // do no keep the reference of data
	if len(self.shards) >= kRSSenderMaxDataShard {
		self.Flush(ctx)
	}
}

const kRSSenderLossPercentMax = 50

var gParityMap [kRSSenderLossPercentMax][kRSSenderMaxDataShard]uint8

func getParityNumber(dataShards int, loss float64) int {
	lossIdx := int(math.Ceil(loss * 100))
	if lossIdx < 1 {
		lossIdx = 1
	}
	if lossIdx > kRSSenderLossPercentMax {
		lossIdx = kRSSenderLossPercentMax
	}
	return int(gParityMap[lossIdx-1][dataShards-1])
}

func getEncoder(cache *map[uint16]reedsolomon.Encoder, nData int, nParity int) reedsolomon.Encoder {
	key := (uint16(nData) << 8) | uint16(nParity)
	coder := (*cache)[key]
	if coder == nil {
		var err error
		coder, err = reedsolomon.New(
			nData, nParity,
			reedsolomon.WithFastOneParityMatrix(),
			reedsolomon.WithCauchyMatrix(),
			reedsolomon.WithInversionCache(false), // too much memory
		)
		assert(err == nil)

		if *cache == nil {
			*cache = map[uint16]reedsolomon.Encoder{}
		}
		(*cache)[key] = coder
	}
	return coder
}

func (self *RSSender) Flush(ctx context.Context) {
	if len(self.shards) == 0 {
		return
	}

	// build coder
	nData := len(self.shards)
	nParity := getParityNumber(nData, self.Loss)
	assert(nData+nParity <= 256)
	coder := getEncoder(&self.encoders, nData, nParity)

	// padding
	blockSize := 0
	for _, block := range self.shards {
		if len(block) > blockSize {
			blockSize = len(block)
		}
	}
	for i, block := range self.shards {
		self.shards[i] = append(block, make([]byte, blockSize-len(block))...)
	}

	// log
	if self.Debug {
		ctxlog.Debugf(ctx, "[RSSender::Flush] [group:%v] [n_data:%v][n_parity:%v] [block_size:%v]",
			self.groupId, nData, nParity, blockSize)
	}

	// make headers
	parityShards := make([][]byte, nParity) // with header
	for i := range parityShards {
		shard := make([]byte, 8+blockSize)
		binary.LittleEndian.PutUint32(shard[0:4], self.groupId)
		shard[4] = byte(len(self.shards)) // idx
		shard[5] = byte(nData)            // n_data
		shard[6] = byte(nParity)          // n_parity
		shard[7] = 0                      // reserved
		parityShards[i] = shard
		self.shards = append(self.shards, shard[8:]) // no header
	}

	// encoding
	err := coder.Encode(self.shards)
	assert(err == nil)

	// output
	for _, shard := range parityShards {
		self.Output(ctx, shard)

		self.Stats.totalBytes += int64(len(shard)) - 8 // no header
		self.Stats.totalParityBytes += int64(len(shard)) - 8
		self.Stats.totalParityShards++
	}
	self.Stats.totalGroups++

	// finishing
	self.groupId++
	self.shards = self.shards[:0]
}

const (
	kGroupStateInit    = 0
	kGroupStatePending = 1
	kGroupStateDone    = 2
)

type rsGroup struct {
	groupId uint32
	state   uint32
	shards  [][]byte
}

type RSReceiver struct {
	// params
	Output func(context.Context, []byte)
	Debug  bool
	// output
	Stats RSReceiverStats
	// private
	window   []rsGroup
	encoders map[uint16]reedsolomon.Encoder
}

type RSReceiverStats struct {
	// memory
	cachedGroups int64
	cachedShards int64
	cachedBytes  int64
	// counters
	totalGroups          int64
	totalInputShards     int64 // including dup and parity
	totalOutputShards    int64
	totalDupShards       int64
	totalRecoveredShards int64
}

const kRSReceiverWindowSize = 4096
const kRSReceiverWindowMask = kRSReceiverWindowSize - 1

func (self *RSReceiver) Input(ctx context.Context, shard []byte) error {
	if self.Debug {
		ctx = ctxlog.Push(ctx, "[RSReceiver::Input]")
	}

	// parse
	if len(shard) < 8 {
		return errors.New("len(shard) < 8")
	}
	groupId := binary.LittleEndian.Uint32(shard[0:4])
	idx := int(shard[4])
	nData := int(shard[5])
	nParity := int(shard[6])
	payload := shard[8:]
	if nData == 0 && idx > kRSSenderMaxDataShard {
		return errors.New("idx > kRSSenderMaxDataShard")
	}
	if nData+nParity > 256 {
		return errors.New("nData + nParity > 256")
	}
	if nData > 0 && idx >= nData+nParity {
		return errors.New("nData > 0 && idx >= nData + nParity")
	}
	if nData > 0 && nParity == 0 {
		return errors.New("nData > 0 && nParity == 0")
	}
	if nData == 0 && nParity != 0 {
		return errors.New("nData == 0 && nParity != 0")
	}

	// stats
	self.Stats.totalInputShards++

	// check window
	if self.window == nil {
		self.window = make([]rsGroup, kRSReceiverWindowSize)
	}
	group := &self.window[groupId&kRSReceiverWindowMask]

	// new group
	if group.groupId != groupId || group.state == kGroupStateInit {
		if group.state == kGroupStatePending {
			if self.Debug {
				ctxlog.Debugf(ctx, "[group:%v] unfinished, overwritting now", group.groupId)
			}
			self.clearGroup(group) // remove previous unrecovered group
		}
		self.Stats.cachedGroups++
		self.Stats.totalGroups++
		*group = rsGroup{
			groupId: groupId,
			state:   kGroupStatePending,
			shards:  nil,
		}
	}

	// ignore finished group
	if group.state == kGroupStateDone {
		if self.Debug {
			ctxlog.Debugf(ctx, "[group:%v] [idx:%v] ignore finished group (dup or parity)", groupId, idx)
		}
		return nil
	}

	// insert shard to group
	if idx >= len(group.shards) {
		group.shards = append(group.shards, make([][]byte, idx-len(group.shards)+1)...)
	}
	if len(group.shards[idx]) > 0 {
		if self.Debug {
			ctxlog.Debugf(ctx, "[group:%v] [idx:%v] ignore dup", groupId, idx)
		}
		self.Stats.totalDupShards++
		return nil // ignore dup
	}
	group.shards[idx] = append([]byte(nil), shard...) // an extra copy, do not keep the reference of the input slice
	self.Stats.cachedShards++
	self.Stats.cachedBytes += int64(len(shard))

	// if this is data shard, output immediately
	if nData == 0 {
		if self.Debug {
			ctxlog.Debugf(ctx, "[group:%v] [idx:%v] got data shard", groupId, idx)
		}
		self.Output(ctx, payload)
		self.Stats.totalOutputShards++
	}

	// load group configuration from parity shard
	lastShard := group.shards[len(group.shards)-1]
	nData = int(lastShard[5])   // n_data
	nParity = int(lastShard[6]) // n_parity

	// no parity shard yet (current shard is data shard)
	if nData == 0 {
		return nil
	}

	// this is a parity shard
	if self.Debug {
		ctxlog.Debugf(ctx, "[group:%v][n_data:%v][n_parity:%v] [idx:%v] got parity shard",
			groupId, nData, nParity, idx)
	}

	// try to recover data shard
	missingCnt := nData + nParity
	missingIdx := []int(nil)
	blockSize := 0
	for i := 0; i < len(group.shards); i++ {
		if len(group.shards[i]) > 0 {
			missingCnt--
		} else if i < nData {
			missingIdx = append(missingIdx, i)
		}
		if len(group.shards[i]) > blockSize {
			blockSize = len(group.shards[i])
		}
	}
	blockSize -= 8 // no header

	// do recovery
	if len(missingIdx) > 0 && missingCnt <= nParity {
		assert(missingCnt == nParity)
		if self.Debug {
			ctxlog.Debugf(ctx, "[group:%v] [n_data:%v][n_parity:%v] recovering %v data shard",
				groupId, nData, nParity, len(missingIdx))
		}

		// padding
		padded := make([][]byte, nData+nParity)
		for i := 0; i < nData+nParity; i++ {
			block := []byte(nil) // nil denotes missing shard
			if i < len(group.shards) && len(group.shards[i]) > 0 {
				block = group.shards[i][8:] // no header
			}
			if block != nil && len(block) < blockSize {
				block = append(block, make([]byte, blockSize-len(block))...)
			}
			padded[i] = block
		}

		// encoding
		coder := getEncoder(&self.encoders, nData, nParity)
		err := coder.ReconstructData(padded)
		if err != nil {
			return err // XXX: what can go wrong? matrix not inversible?
		}

		// output data shards
		for _, idx := range missingIdx {
			self.Output(ctx, padded[idx])
			self.Stats.totalOutputShards++
		}
		self.Stats.totalRecoveredShards += int64(len(missingIdx))
		missingIdx = nil // done
	}

	// mark finished group and release memory
	if len(missingIdx) == 0 {
		if self.Debug {
			ctxlog.Debugf(ctx, "[group:%v] finished", groupId)
		}
		self.clearGroup(group)
	}
	return nil
}

func (self *RSReceiver) clearGroup(group *rsGroup) {
	self.Stats.cachedGroups--
	for _, shard := range group.shards {
		if len(shard) > 0 {
			self.Stats.cachedShards--
		}
		self.Stats.cachedBytes -= int64(len(shard))
	}
	group.state = kGroupStateDone
	group.shards = nil
}
