//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

type Drive struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
	Kind  string `json:"kind"`
}

func listDrives() []Drive {
	var out []Drive
	for _, p := range []string{"/", "/home"} {
		var st syscall.Statfs_t
		if syscall.Statfs(p, &st) == nil {
			out = append(out, Drive{Path: p, Label: p, Total: int64(st.Blocks) * st.Bsize, Free: int64(st.Bavail) * st.Bsize, Kind: "ssd"})
		}
	}
	return out
}

func moveToRecycleBin(path string) error {
	return errors.New("moving to the Recycle Bin is only supported on Windows")
}

func revealInExplorer(path string) error { return exec.Command("xdg-open", path).Start() }

func openWindow(url string) {}

func fastScan(path string, s *Scanner) (*Node, error) { return nil, nil }
