//go:build !windows

package pathsecurity

import (
	"os"
	"syscall"
)

func owner(info os.FileInfo) (uint32, uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, uint32(os.Geteuid()), false
	}
	return stat.Uid, uint32(os.Geteuid()), true
}
