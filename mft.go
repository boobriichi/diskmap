package main

// Fast NTFS scanner: reads the Master File Table ($MFT) of a volume directly,
// the same trick WizTree uses. Instead of asking the OS about every folder,
// we read one big table that already lists every file with its size and parent.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
)

type mftRec struct {
	name   string
	parent uint32
	size   int64
	inUse  bool
	isDir  bool
	nsPri  uint8 // 0 = no name yet; higher = better name (Win32 > POSIX > DOS 8.3)
}

type mftExt struct {
	base    uint32
	size    int64
	hasSize bool
	name    string
	parent  uint32
	nsPri   uint8
}

type dataRun struct{ lcn, clusters int64 }

var errNotNTFS = errors.New("volume is not NTFS")

func nsPriority(ns uint8) uint8 {
	switch ns {
	case 1, 3: // Win32, Win32&DOS
		return 3
	case 0: // POSIX
		return 2
	default: // DOS 8.3 short name
		return 1
	}
}

// applyFixup restores the last two bytes of every sector (NTFS "update sequence array").
func applyFixup(rec []byte, sector int) bool {
	usaOff := int(binary.LittleEndian.Uint16(rec[4:]))
	usaCnt := int(binary.LittleEndian.Uint16(rec[6:]))
	if usaOff+usaCnt*2 > len(rec) || usaCnt == 0 {
		return false
	}
	u0, u1 := rec[usaOff], rec[usaOff+1]
	for i := 1; i < usaCnt; i++ {
		p := i*sector - 2
		if p+1 >= len(rec) {
			break
		}
		if rec[p] != u0 || rec[p+1] != u1 {
			return false
		}
		rec[p], rec[p+1] = rec[usaOff+2*i], rec[usaOff+2*i+1]
	}
	return true
}

func decodeRuns(b []byte) []dataRun {
	var runs []dataRun
	var lcn int64
	for i := 0; i < len(b) && b[i] != 0; {
		h := b[i]
		ls, os := int(h&0x0f), int(h>>4)
		i++
		if i+ls+os > len(b) || ls == 0 || ls > 8 || os > 8 {
			break
		}
		var length int64
		for k := ls - 1; k >= 0; k-- {
			length = length<<8 | int64(b[i+k])
		}
		i += ls
		if os == 0 { // sparse run
			runs = append(runs, dataRun{-1, length})
			continue
		}
		off := int64(int8(b[i+os-1])) // sign-extend highest byte
		for k := os - 2; k >= 0; k-- {
			off = off<<8 | int64(b[i+k])
		}
		i += os
		lcn += off
		runs = append(runs, dataRun{lcn, length})
	}
	return runs
}

// parseRecord fills rec (base records) or returns an extension (records that belong to another base record).
func parseRecord(buf []byte, sector int, out *mftRec) (ext *mftExt, runs []dataRun, ok bool) {
	if len(buf) < 48 || string(buf[0:4]) != "FILE" || !applyFixup(buf, sector) {
		return nil, nil, false
	}
	flags := binary.LittleEndian.Uint16(buf[0x16:])
	if flags&1 == 0 {
		return nil, nil, false
	}
	baseRef := binary.LittleEndian.Uint64(buf[0x20:]) & 0xFFFFFFFFFFFF
	var r mftRec
	r.inUse = true
	r.isDir = flags&2 != 0
	var hasSize bool

	pos := int(binary.LittleEndian.Uint16(buf[0x14:]))
	used := int(binary.LittleEndian.Uint32(buf[0x18:]))
	if used > len(buf) {
		used = len(buf)
	}
	for pos+16 <= used {
		typ := binary.LittleEndian.Uint32(buf[pos:])
		if typ == 0xFFFFFFFF {
			break
		}
		alen := int(binary.LittleEndian.Uint32(buf[pos+4:]))
		if alen < 16 || pos+alen > used {
			break
		}
		a := buf[pos : pos+alen]
		nonRes := a[8] != 0
		nameLen := a[9]
		switch typ {
		case 0x30: // $FILE_NAME (always resident)
			if nonRes || alen < 24 {
				break
			}
			vOff := int(binary.LittleEndian.Uint16(a[0x14:]))
			vLen := int(binary.LittleEndian.Uint32(a[0x10:]))
			if vOff+vLen > alen || vLen < 0x42 {
				break
			}
			v := a[vOff : vOff+vLen]
			nl := int(v[0x40])
			pri := nsPriority(v[0x41])
			if 0x42+nl*2 > len(v) || pri <= r.nsPri {
				break
			}
			u := make([]uint16, nl)
			for k := range u {
				u[k] = binary.LittleEndian.Uint16(v[0x42+k*2:])
			}
			r.name = string(utf16.Decode(u))
			r.parent = uint32(binary.LittleEndian.Uint64(v[0:]) & 0xFFFFFFFFFFFF)
			r.nsPri = pri
		case 0x80: // $DATA, only the unnamed (main) stream
			if nameLen != 0 {
				break
			}
			if !nonRes {
				r.size = int64(binary.LittleEndian.Uint32(a[0x10:]))
				hasSize = true
			} else if alen >= 0x40 && binary.LittleEndian.Uint64(a[0x10:]) == 0 { // first extent holds the size
				r.size = int64(binary.LittleEndian.Uint64(a[0x30:]))
				hasSize = true
				roff := int(binary.LittleEndian.Uint16(a[0x20:]))
				if roff < alen {
					runs = decodeRuns(a[roff:])
				}
			}
		}
		pos += alen
	}
	if baseRef != 0 {
		return &mftExt{base: uint32(baseRef), size: r.size, hasSize: hasSize, name: r.name, parent: r.parent, nsPri: r.nsPri}, nil, true
	}
	*out = r
	return nil, runs, true
}

// scanMFT reads an NTFS volume (raw device) and builds the folder tree.
func scanMFT(vol io.ReaderAt, rootName string, s *Scanner) (*Node, error) {
	boot := make([]byte, 512)
	if _, err := vol.ReadAt(boot, 0); err != nil {
		return nil, err
	}
	if string(boot[3:11]) != "NTFS    " {
		return nil, errNotNTFS
	}
	sector := int(binary.LittleEndian.Uint16(boot[0x0B:]))
	cluster := int64(sector) * int64(boot[0x0D])
	mftLCN := int64(binary.LittleEndian.Uint64(boot[0x30:]))
	cpr := int8(boot[0x40])
	recSize := int64(cpr) * cluster
	if cpr < 0 {
		recSize = 1 << uint(-cpr)
	}
	if sector < 256 || cluster <= 0 || recSize < 256 || recSize > 65536 {
		return nil, errors.New("unexpected NTFS boot sector")
	}

	// Record 0 is $MFT itself: its $DATA runs tell where the whole table lives.
	readSize := recSize
	if readSize < int64(sector) {
		readSize = int64(sector)
	}
	r0 := make([]byte, readSize)
	if _, err := vol.ReadAt(r0, mftLCN*cluster); err != nil {
		return nil, err
	}
	var self mftRec
	_, runs, ok := parseRecord(r0[:recSize], sector, &self)
	if !ok || len(runs) == 0 || self.size <= 0 {
		return nil, errors.New("could not read the $MFT record")
	}
	total := self.size / recSize
	recs := make([]mftRec, total)

	type job struct {
		idx int64
		buf []byte
	}
	jobs := make(chan job, 8)
	var extMu sync.Mutex
	var exts []mftExt
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	bufPool := sync.Pool{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []mftExt
			for j := range jobs {
				n := int64(len(j.buf)) / recSize
				for k := int64(0); k < n; k++ {
					idx := j.idx + k
					if idx >= total {
						break
					}
					e, _, ok := parseRecord(j.buf[k*recSize:(k+1)*recSize], sector, &recs[idx])
					if ok && e != nil {
						local = append(local, *e)
					}
					if ok && e == nil && !recs[idx].isDir {
						s.files.Add(1)
						s.bytes.Add(recs[idx].size)
					}
				}
				bufPool.Put(j.buf[:cap(j.buf)])
			}
			extMu.Lock()
			exts = append(exts, local...)
			extMu.Unlock()
		}()
	}

	const chunk = 8 << 20 // 8 MB reads
	var idx int64
	var readErr error
read:
	for _, run := range runs {
		if run.lcn < 0 { // sparse part of MFT: no records there
			idx += run.clusters * cluster / recSize
			continue
		}
		off := run.lcn * cluster
		left := run.clusters * cluster
		for left > 0 && idx < total {
			if s.cancel.Load() {
				break read
			}
			n := int64(chunk)
			if n > left {
				n = left
			}
			if rem := (total - idx) * recSize; n > rem { // don't read past the table
				n = (rem + cluster - 1) / cluster * cluster
			}
			var buf []byte
			if b, ok := bufPool.Get().([]byte); ok && int64(cap(b)) >= n {
				buf = b[:n]
			} else {
				buf = make([]byte, chunk)[:n]
			}
			if _, err := vol.ReadAt(buf, off); err != nil && err != io.EOF {
				readErr = err
				break read
			}
			jobs <- job{idx, buf}
			idx += n / recSize
			off += n
			left -= n
			s.current.Store(fmt.Sprintf("Reading MFT: %d%%", idx*100/total))
		}
	}
	close(jobs)
	wg.Wait()
	if readErr != nil && idx == 0 {
		return nil, readErr
	}

	// Merge extension records (big/fragmented files keep data in extra records).
	for _, e := range exts {
		if int64(e.base) >= total {
			continue
		}
		b := &recs[e.base]
		if !b.inUse {
			continue
		}
		if e.hasSize {
			b.size = e.size
			if !b.isDir {
				s.bytes.Add(e.size)
			}
		}
		if e.nsPri > b.nsPri {
			b.name, b.parent, b.nsPri = e.name, e.parent, e.nsPri
		}
	}

	// Build the tree: first all folders, then hang files on them.
	s.current.Store("Building tree…")
	const rootRec = 5
	if total <= rootRec || !recs[rootRec].isDir {
		return nil, errors.New("root folder missing in MFT")
	}
	dirs := make([]*Node, total)
	for i := range recs {
		r := &recs[i]
		if r.inUse && r.isDir && (r.nsPri > 0 || i == rootRec) {
			dirs[i] = &Node{Name: r.name, IsDir: true}
		}
	}
	root := dirs[rootRec]
	root.Name = rootName
	for i, d := range dirs {
		if d == nil || i == rootRec {
			continue
		}
		p := recs[i].parent
		if int64(p) >= total || int(p) == i || dirs[p] == nil {
			continue
		}
		d.Parent = dirs[p]
		dirs[p].Children = append(dirs[p].Children, d)
	}
	for i := range recs {
		r := &recs[i]
		if !r.inUse || r.isDir || r.nsPri == 0 {
			continue
		}
		if int64(r.parent) >= total || dirs[r.parent] == nil {
			continue
		}
		p := dirs[r.parent]
		if r.size >= smallFileLimit {
			p.Children = append(p.Children, &Node{Name: r.name, Size: r.size, Files: 1, Parent: p})
		} else {
			p.SmallSize += r.size
			p.SmallCount++
		}
	}
	return root, nil
}

// subtree finds a folder inside a freshly built volume tree (case-insensitive, like Windows).
func subtree(root *Node, rel string) *Node {
	n := root
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '\\' || r == '/' }) {
		var next *Node
		for _, c := range n.Children {
			if c.IsDir && strings.EqualFold(c.Name, part) {
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
