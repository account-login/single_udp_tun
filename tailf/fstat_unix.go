//+build !windows

package tailf

import "syscall"

func Fstat(fd uintptr, stat *StatResult) error {
	r := syscall.Stat_t{}
	err := syscall.Fstat(int(fd), &r)
	if err != nil {
		return err
	}

	stat.Size = r.Size
	stat.Inode = r.Ino
	return nil
}
