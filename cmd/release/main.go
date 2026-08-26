package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

type target struct{ OS, Arch string }

func main() {
	version := flag.String("version", "", "release version")
	out := flag.String("out", "dist", "output directory")
	flag.Parse()
	if *version == "" {
		fatal(fmt.Errorf("--version is required"))
	}
	if err := build(*version, *out); err != nil {
		fatal(err)
	}
}
func build(version, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	targets := []target{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}
	var checks []string
	for _, t := range targets {
		tmp, err := os.MkdirTemp("", "workrun-release-")
		if err != nil {
			return err
		}
		bin := filepath.Join(tmp, "workrun")
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", bin, "./cmd/workrun")
		cmd.Env = append(os.Environ(), "GOOS="+t.OS, "GOARCH="+t.Arch, "CGO_ENABLED=0")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Run(); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		name := fmt.Sprintf("workrun_%s_%s_%s.tar.gz", version, t.OS, t.Arch)
		archive := filepath.Join(out, name)
		if err = pack(archive, bin); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		sum, err := checksum(archive)
		os.RemoveAll(tmp)
		if err != nil {
			return err
		}
		checks = append(checks, sum+"  "+name)
	}
	sort.Strings(checks)
	content := ""
	for _, line := range checks {
		content += line + "\n"
	}
	return os.WriteFile(filepath.Join(out, "checksums.txt"), []byte(content), 0o644)
}
func pack(path, bin string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz := gzip.NewWriter(file)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	info, err := os.Stat(bin)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = "workrun"
	hdr.Mode = 0o755
	if err = tw.WriteHeader(hdr); err != nil {
		return err
	}
	src, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(tw, src)
	return err
}
func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
