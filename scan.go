package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Files smaller than this are not stored individually; they are summed
// into their folder's "small files" bucket to keep memory low on huge disks.
const smallFileLimit = 256 * 1024

type Node struct {
	Name       string
	Size       int64
	Files      int64 // number of files in subtree
	IsDir      bool
	Children   []*Node
	Parent     *Node
	SmallSize  int64
	SmallCount int64
	mu         sync.Mutex
}

func (n *Node) Path() string {
	if n.Parent == nil {
		return n.Name
	}
	return filepath.Join(n.Parent.Path(), n.Name)
}

type Scanner struct {
	mu       sync.RWMutex
	root     *Node
	rootPath string
	scanning bool
	files    atomic.Int64
	bytes    atomic.Int64
	errors   atomic.Int64
	current  atomic.Value // string
	mode     atomic.Value // string
	fastErr  atomic.Value // string
	took     atomic.Int64
	cancel   atomic.Bool
}

func NewScanner() *Scanner {
	s := &Scanner{}
	s.current.Store("")
	s.mode.Store("")
	s.fastErr.Store("")
	return s
}

func (s *Scanner) Start(path string) {
	path = filepath.Clean(path)
	if len(path) == 2 && path[1] == ':' { // "C:" -> "C:\"
		path += string(filepath.Separator)
	}
	s.mu.Lock()
	if s.scanning {
		s.cancel.Store(true)
	}
	s.mu.Unlock()

	go func() {
		s.mu.Lock()
		for s.scanning { // wait for previous scan to stop
			s.mu.Unlock()
			sleepMs(20)
			s.mu.Lock()
		}
		s.scanning = true
		s.cancel.Store(false)
		s.files.Store(0)
		s.bytes.Store(0)
		s.errors.Store(0)
		s.root = nil
		s.rootPath = path
		s.mu.Unlock()

		start := time.Now()
		root, err := fastScan(path, s) // NTFS MFT reader on Windows (needs admin)
		if root != nil {
			s.mode.Store("MFT")
		} else {
			if err != nil {
				log.Println("fast scan unavailable:", err)
				s.fastErr.Store(err.Error())
			}
			s.mode.Store("directory walk")
			root = &Node{Name: path, IsDir: true}
			sem := make(chan struct{}, 64)
			var wg sync.WaitGroup
			s.walk(root, path, sem, &wg)
			wg.Wait()
		}
		sumSizes(root)
		s.took.Store(time.Since(start).Milliseconds())

		s.mu.Lock()
		s.root = root
		s.scanning = false
		s.mu.Unlock()
	}()
}

func (s *Scanner) walk(n *Node, path string, sem chan struct{}, wg *sync.WaitGroup) {
	if s.cancel.Load() {
		return
	}
	s.current.Store(path)
	entries, err := os.ReadDir(path)
	if err != nil {
		s.errors.Add(1)
		return
	}
	for _, e := range entries {
		t := e.Type()
		// skip symlinks / junctions / mount points to avoid loops and double counting
		if t&os.ModeSymlink != 0 || t&os.ModeIrregular != 0 {
			continue
		}
		if e.IsDir() {
			child := &Node{Name: e.Name(), IsDir: true, Parent: n}
			n.mu.Lock()
			n.Children = append(n.Children, child)
			n.mu.Unlock()
			childPath := filepath.Join(path, e.Name())
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.walk(child, childPath, sem, wg)
					<-sem
				}()
			default:
				s.walk(child, childPath, sem, wg)
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			s.errors.Add(1)
			continue
		}
		size := info.Size()
		s.files.Add(1)
		s.bytes.Add(size)
		n.mu.Lock()
		if size >= smallFileLimit {
			n.Children = append(n.Children, &Node{Name: e.Name(), Size: size, Files: 1, Parent: n})
		} else {
			n.SmallSize += size
			n.SmallCount++
		}
		n.mu.Unlock()
	}
}

func sumSizes(n *Node) {
	if !n.IsDir {
		return
	}
	size, files := n.SmallSize, n.SmallCount
	for _, c := range n.Children {
		sumSizes(c)
		size += c.Size
		files += c.Files
	}
	n.Size, n.Files = size, files
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Size > n.Children[j].Size })
}

// Find returns the node for an absolute path inside the scanned tree.
func (s *Scanner) Find(path string) *Node {
	if s.root == nil {
		return nil
	}
	path = filepath.Clean(path)
	rootClean := filepath.Clean(s.rootPath)
	if strings.EqualFold(path, rootClean) || path == "" {
		return s.root
	}
	rel, err := filepath.Rel(rootClean, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	n := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		var next *Node
		for _, c := range n.Children {
			if c.Name == part {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// Remove detaches a node and subtracts its size from all ancestors.
func (s *Scanner) Remove(n *Node) {
	p := n.Parent
	if p == nil {
		return
	}
	for i, c := range p.Children {
		if c == n {
			p.Children = append(p.Children[:i], p.Children[i+1:]...)
			break
		}
	}
	for a := p; a != nil; a = a.Parent {
		a.Size -= n.Size
		a.Files -= n.Files
	}
	sort.Slice(p.Children, func(i, j int) bool { return p.Children[i].Size > p.Children[j].Size })
}

// ---- JSON output ----

type OutNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path,omitempty"`
	Size     int64      `json:"size"`
	Files    int64      `json:"files"`
	Kind     string     `json:"kind"` // dir | file | small | other
	Children []*OutNode `json:"children,omitempty"`
}

// Export builds a trimmed subtree for drawing: depth levels, tiny slices merged.
func Export(n *Node, path string, depth int, rootSize int64, level int) *OutNode {
	o := &OutNode{Name: n.Name, Path: path, Size: n.Size, Files: n.Files, Kind: "file"}
	if n.IsDir {
		o.Kind = "dir"
	}
	if !n.IsDir || depth == 0 {
		return o
	}
	minSize := rootSize / 600 // ~0.6° of the full circle
	maxKids := 400
	if level > 0 {
		maxKids = 60
	}
	var otherSize, otherFiles, otherCount int64
	for _, c := range n.Children {
		if c.Size <= 0 {
			continue
		}
		if len(o.Children) >= maxKids || (level > 0 && c.Size < minSize) {
			otherSize += c.Size
			otherFiles += c.Files
			otherCount++
			continue
		}
		o.Children = append(o.Children, Export(c, filepath.Join(path, c.Name), depth-1, rootSize, level+1))
	}
	if n.SmallSize > 0 {
		o.Children = append(o.Children, &OutNode{Name: "small files (" + itoa(n.SmallCount) + ")", Size: n.SmallSize, Files: n.SmallCount, Kind: "small"})
	}
	if otherSize > 0 {
		o.Children = append(o.Children, &OutNode{Name: itoa(otherCount) + " more items", Size: otherSize, Files: otherFiles, Kind: "other"})
	}
	sort.Slice(o.Children, func(i, j int) bool { return o.Children[i].Size > o.Children[j].Size })
	return o
}
