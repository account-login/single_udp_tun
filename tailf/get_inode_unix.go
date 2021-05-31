//+build !windows

package tailf

import "syscall"

func FGetInode(fd uintptr) (uint64, error) {
	r := syscall.Stat_t{}
	err := syscall.Fstat(int(fd), &r)
	if err != nil {
		return 0, err
	}

	return r.Ino, nil
}
