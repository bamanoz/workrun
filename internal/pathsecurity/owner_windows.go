//go:build windows

package pathsecurity

import "os"

func owner(os.FileInfo) (uint32, uint32, bool) {
	return 0, 0, false
}
