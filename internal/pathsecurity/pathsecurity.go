package pathsecurity

import (
	"fmt"
	"os"
	"runtime"
)

type Info struct {
	Path       string      `json:"path"`
	Mode       os.FileMode `json:"-"`
	ModeText   string      `json:"mode"`
	OwnerUID   uint32      `json:"owner_uid,omitempty"`
	CurrentUID uint32      `json:"current_uid,omitempty"`
	OwnerKnown bool        `json:"owner_known"`
	Directory  bool        `json:"directory"`
	Symlink    bool        `json:"symlink"`
	Safe       bool        `json:"safe"`
	Error      string      `json:"error,omitempty"`
}

func Inspect(path string, directory, private bool) Info {
	result := Info{Path: path, Directory: directory}
	info, err := os.Lstat(path)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Mode = info.Mode()
	result.ModeText = fmt.Sprintf("%04o", info.Mode().Perm())
	result.Symlink = info.Mode()&os.ModeSymlink != 0
	result.OwnerUID, result.CurrentUID, result.OwnerKnown = owner(info)
	if result.Symlink {
		result.Error = "symbolic links are not allowed"
		return result
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		result.Error = "unexpected file type"
		return result
	}
	if runtime.GOOS != "windows" {
		forbidden := os.FileMode(0o022)
		if private {
			forbidden = 0o077
		}
		if info.Mode().Perm()&forbidden != 0 {
			result.Error = fmt.Sprintf("unsafe permissions %04o", info.Mode().Perm())
			return result
		}
		if result.OwnerKnown && result.OwnerUID != result.CurrentUID {
			result.Error = "path is owned by another user"
			return result
		}
	}
	result.Safe = true
	return result
}

func ValidateDir(path string, private bool) error {
	return validate(Inspect(path, true, private))
}

func ValidateFile(path string) error {
	return validate(Inspect(path, false, true))
}

func validate(info Info) error {
	if info.Safe {
		return nil
	}
	if info.Error == "" {
		info.Error = "unsafe path"
	}
	return fmt.Errorf("%s: %s", info.Path, info.Error)
}
