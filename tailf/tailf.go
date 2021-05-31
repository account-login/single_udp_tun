package tailf

import (
	"bufio"
	"context"
	"github.com/account-login/ctxlog"
	"io"
	"os"
	"sync"
	"time"
)

type TailF struct {
	Path   string
	Ctx    context.Context
	Output chan string
}

type NoEOFReader struct {
	r    io.Reader
	done <-chan struct{}
}

func (self NoEOFReader) Read(p []byte) (n int, err error) {
	for n, err = self.r.Read(p); err == io.EOF; {
		time.Sleep(kInterval)
		if self.done != nil {
			_, yes := <-self.done
			if yes {
				break
			}
		}
	}
	return
}

const kInterval = 100 * time.Millisecond

func (self *TailF) Run() {
	var mu sync.Mutex
	var fp *os.File // protected by mu
	var inode uint64
	var suppressLog bool

	defer func() {
		mu.Lock()
		if fp != nil {
			_ = fp.Close()
			fp = nil
		}
		mu.Unlock()
	}()

	done := self.Ctx.Done()
	for {
		// check for cancellation
		if done != nil {
			_, yes := <-done
			if yes {
				break
			}
		}

		mu.Lock()
		cfp := fp
		mu.Unlock()
		if cfp == nil {
			// file not opened, open, seek, fstat
			var err error
			cfp, err = os.Open(self.Path)
			if err != nil {
				if !suppressLog {
					ctxlog.Debugf(self.Ctx, "[tailf] error open [path:%v]: %v", self.Path, err)
					suppressLog = true
				}
				goto LCleanupOpen
			}
			suppressLog = false

			_, err = cfp.Seek(0, io.SeekEnd)
			if err != nil {
				ctxlog.Errorf(self.Ctx, "[tailf] error seek [path:%v]: %v", self.Path, err)
				goto LCleanupOpen
			}

			inode, err = FGetInode(cfp.Fd())
			if err != nil {
				ctxlog.Errorf(self.Ctx, "[tailf] error get inode [path:%v]: %v", self.Path, err)
				goto LCleanupOpen
			}
			ctxlog.Debugf(self.Ctx, "[tailf] opened [path:%v][fd:%v][ino:%v]", self.Path, cfp.Fd(), inode)

			// ok, start reading file
			mu.Lock()
			fp = cfp
			mu.Unlock()
			go func(cfp *os.File) {
				scanner := bufio.NewScanner(NoEOFReader{r: cfp, done: done})
				for scanner.Scan() {
					self.Output <- scanner.Text()
				}
				err := scanner.Err() // don't care
				ctxlog.Infof(self.Ctx, "[tailf] reader [fd:%v] exited with err: %v", cfp.Fd(), err)

				// reset
				mu.Lock()
				if fp == cfp {
					_ = cfp.Close()
					fp = nil
				}
				mu.Unlock()
			}(cfp)

		LCleanupOpen:
			if err != nil {
				_ = cfp.Close()
			}
		} else {
			// monitor the fp
			var minode uint64
			mfp, err := os.Open(self.Path)
			if err != nil {
				if !suppressLog {
					ctxlog.Debugf(self.Ctx, "[tailf] error open [path:%v]: %v", self.Path, err)
					suppressLog = true
				}
				goto LCleanupMon
			}
			suppressLog = false

			minode, err = FGetInode(cfp.Fd())
			if err != nil {
				ctxlog.Errorf(self.Ctx, "[tailf] error get inode [path:%v]: %v", self.Path, err)
				goto LCleanupMon
			}
			if inode != minode {
				ctxlog.Infof(
					self.Ctx, "[tailf] [path:%v] inode changed [from:%v] to [to:%v]",
					self.Path, inode, minode,
				)
				// new file at path
				mu.Lock()
				if fp == cfp {
					_ = cfp.Close()
					fp = nil
				}
				mu.Unlock()
				continue // try next
			}

		LCleanupMon:
			if mfp != nil {
				_ = mfp.Close()
			}
		}

		time.Sleep(kInterval)
	} // just loop
}
