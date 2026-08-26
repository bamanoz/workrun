package pathsecurity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrivatePathsRejectBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions are not available")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDir(dir, true); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("private group-readable directory accepted: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte("config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(file); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("private group-readable file accepted: %v", err)
	}
}

func TestPathsRejectSymbolicLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges")
	}
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(root, "dir-link")
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDir(dirLink, true); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("directory symlink accepted: %v", err)
	}
	targetFile := filepath.Join(targetDir, "state.db")
	if err := os.WriteFile(targetFile, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(root, "file-link")
	if err := os.Symlink(targetFile, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(fileLink); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("file symlink accepted: %v", err)
	}
}
