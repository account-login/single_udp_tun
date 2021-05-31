//+build windows

package tailf

import (
	"golang.org/x/sys/windows"
)

func FGetInode(fd uintptr) (uint64, error) {
	r := windows.ByHandleFileInformation{}
	err := windows.GetFileInformationByHandle(windows.Handle(fd), &r)
	if err != nil {
		return 0, err
	}

	return (uint64(r.FileIndexHigh) << 32) | uint64(r.FileIndexLow), nil
}
