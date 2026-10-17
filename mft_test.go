package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestMFTImage(t *testing.T) {
	f, err := os.Open("/tmp/ntfs.img")
	if err != nil {
		t.Skip("no image")
	}
	defer f.Close()
	s := NewScanner()
	st := time.Now()
	root, err := scanMFT(f, `X:\`, s)
	if err != nil {
		t.Fatal(err)
	}
	sumSizes(root)
	var size, files int64
	for _, c := range root.Children {
		if strings.HasPrefix(c.Name, "$") || c.Name == "System Volume Information" {
			t.Logf("meta %s %d", c.Name, c.Size)
			continue
		}
		size += c.Size
		files += c.Files
	}
	t.Logf("took %v, user size %d files %d", time.Since(st), size, files)
	if size != 6204534830 || files != 12001 {
		t.Fatalf("mismatch: size %d files %d", size, files)
	}
	n := subtree(root, `DIR3\sub3\привет мир`)
	if n == nil || n.Files != 300 {
		t.Fatalf("subtree failed: %+v", n)
	}
}
