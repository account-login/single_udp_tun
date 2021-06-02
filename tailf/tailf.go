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
	fp   *os.File
	done <-chan struct{}
}

func (self NoEOFReader) Read(p []byte) (n int, err error) {
	fp := self.fp
	for n, err = fp.Read(p); err == io.EOF; n, err = fp.Read(p) {
		// get current offset
		var off int64
		off, err = fp.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		// get file size
		var stat StatResult
		err = Fstat(fp.Fd(), &stat)
		if err != nil {
			return 0, err
		}
		if stat.Size < off {
			// file truncated, re-seek to end, will skip data here
			_, err = fp.Seek(0, io.SeekEnd)
			if err != nil {
				return 0, err
			}
		}

		// cancellation
		select {
		case <-time.After(kInterval):
			continue
		case <-self.done:
			return
		}
	}
	return
}

const kInterval = 100 * time.Millisecond

func (self *TailF) Run() {
	var mu sync.Mutex
	var fp *os.File // protected by mu
	var inode uint64
	var dev uint64
	var suppressLog bool
	var stat StatResult

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

			err = Fstat(cfp.Fd(), &stat)
			if err != nil {
				ctxlog.Errorf(self.Ctx, "[tailf] error get inode [path:%v]: %v", self.Path, err)
				goto LCleanupOpen
			}
			dev, inode = stat.Dev, stat.Inode
			ctxlog.Debugf(self.Ctx, "[tailf] opened [path:%v][fd:%v][ino:%v:%v]", self.Path, cfp.Fd(), dev, inode)

			// ok, start reading file
			mu.Lock()
			fp = cfp
			mu.Unlock()
			go func(cfp *os.File, fd uintptr, inode uint64) {
				scanner := bufio.NewScanner(NoEOFReader{fp: cfp, done: done})
				for scanner.Scan() {
					self.Output <- scanner.Text()
				}
				err := scanner.Err() // don't care
				ctxlog.Infof(self.Ctx, "[tailf] reader [prev_fd:%v][inode:%v] exited with err: %v", fd, inode, err)

				// reset
				mu.Lock()
				if fp == cfp {
					_ = cfp.Close()
					fp = nil
				}
				mu.Unlock()
			}(cfp, cfp.Fd(), stat.Inode)

		LCleanupOpen:
			if err != nil {
				_ = cfp.Close()
			}
		} else {
			// monitor the fp
			mfp, err := os.Open(self.Path)
			if err != nil {
				if !suppressLog {
					ctxlog.Debugf(self.Ctx, "[tailf] error open [path:%v]: %v", self.Path, err)
					suppressLog = true
				}
				goto LCleanupMon
			}
			suppressLog = false

			err = Fstat(mfp.Fd(), &stat)
			if err != nil {
				ctxlog.Errorf(self.Ctx, "[tailf] error get inode [path:%v]: %v", self.Path, err)
				goto LCleanupMon
			}
			if dev != stat.Dev || inode != stat.Inode {
				ctxlog.Infof(
					self.Ctx, "[tailf] [path:%v] inode changed [from:%v:%v] to [to:%v:%v]",
					self.Path, dev, inode, stat.Dev, stat.Inode,
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

		// cancellation
		select {
		case <-time.After(kInterval):
			continue
		case <-done:
			return // the reader goroutine will be interrupted either by fp.Close() or the done chan
		}
	} // just loop
}
