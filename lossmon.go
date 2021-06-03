package single_udp_tun

import (
	"context"
	"encoding/json"
	"github.com/account-login/ctxlog"
	"github.com/account-login/single_udp_tun/tailf"
	"net"
	"sync"
)

// parse logs from loss_probe
type LossMon struct {
	// input
	Path string
	// private
	ip2loss sync.Map
}

func (self *LossMon) Get(ip net.IP) (uint32, bool) {
	v, ok := self.ip2loss.Load(string(ip))
	lossp := uint32(0)
	if ok {
		lossp = v.(uint32)
	}
	return lossp, ok
}

func (self *LossMon) Run(ctx context.Context) {
	lines := make(chan string, 1)
	tail := tailf.TailF{
		Path:   self.Path,
		Ctx:    ctx,
		Output: lines,
	}
	go tail.Run()

	work := func(line string) {
		// {"Time":"2021-06-02+23:53:28.813+0800","UnixMS":1622649208813,"ID":"",
		//  "Dst":"127.0.0.1:200","Stats":{"SRTT":169831,"RTTVar":37090,
		//  "Loss1":3,"Late1":0,"Count1":19,"Loss2":3,"Count2":19,"LossP":16}}
		type Item struct {
			Dst   string
			Stats struct {
				LossP uint32
			}
		}
		item := Item{}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			ctxlog.Warnf(ctx, "[LossMon] bad entry: %v", line)
			return
		}

		ipstr, _, err := net.SplitHostPort(item.Dst)
		if err != nil {
			return
		}
		ip := net.ParseIP(ipstr)
		if ip == nil {
			return
		}
		self.ip2loss.Store(string(ip), item.Stats.LossP)
		//ctxlog.Debugf(ctx, "[ip:%v][lossp:%v]", ip, item.Stats.LossP)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case line := <-lines:
			work(line)
		}
	}
}
