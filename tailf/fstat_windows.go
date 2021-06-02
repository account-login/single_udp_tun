//+build windows

package tailf

import (
	"golang.org/x/sys/windows"
)

func Fstat(fd uintptr, stat *StatResult) error {
	r := windows.ByHandleFileInformation{}
	err := windows.GetFileInformationByHandle(windows.Handle(fd), &r)
	if err != nil {
		return err
	}

	stat.Size = int64((uint64(r.FileSizeHigh) << 32) | uint64(r.FileSizeLow))
	stat.Dev = uint64(r.VolumeSerialNumber)
	stat.Inode = (uint64(r.FileIndexHigh) << 32) | uint64(r.FileIndexLow)
	return nil
}
