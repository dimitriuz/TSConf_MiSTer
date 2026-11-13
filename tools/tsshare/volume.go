package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	sectorSize       = 512
	minReserved      = 32 // sectors; padded so the data area is cluster aligned
	minClusters      = 65525
	maxClusters      = 0x0FFFFFF5
	maxDirEntries    = 65536 // FAT32 directory limit (2 MiB)
	maxFileSize      = 1<<32 - 1
	endOfChain       = 0x0FFFFFFF
	defaultLabel     = "TSSHARE"
	defaultPartStart = 128
)

// VolumeOptions tunes NewVolume.
type VolumeOptions struct {
	Label             string // up to 11 chars, upper-cased; default TSSHARE
	PartStartSector   int    // default 128
	SectorsPerCluster int    // 0 = choose from the content
}

// dirExtent is one directory: its encoded bytes and its clusters.
type dirExtent struct {
	start uint32
	count uint32
	data  []byte // entries only; the rest of the extent reads as zeros
	// build-time only
	node   *Node
	parent int32
	refs   []int32 // per child short entry: >=0 dir index, <0 ^file index
}

// fileExtent is one file's contiguous clusters (count 0 for empty files).
type fileExtent struct {
	start uint32
	count uint32
	size  uint32
	node  *Node
}

// Volume is a read-only FAT32 disk synthesized from a Source. It is safe for
// concurrent use after NewVolume returns.
type Volume struct {
	src Source

	spc           int
	partStart     int64
	partSectors   int64
	fatSectors    int64
	dataStart     int64 // partition-relative sector of cluster 2
	totalClusters uint32
	usedEnd       uint32 // first unused cluster number
	size          int64

	head     []byte // MBR sector + gap to partition start is zero; this is sector 0 only
	rsvd     []byte // the reserved sectors of the partition
	reserved int64  // reserved sector count (>= minReserved)
	dirs     []dirExtent
	files    []fileExtent
}

// Size is the virtual disk size in bytes.
func (v *Volume) Size() int64 { return v.size }

// Close closes the source.
func (v *Volume) Close() error { return v.src.Close() }

// ---------------------------------------------------------------- build

type dirChild struct {
	node  *Node
	clean string
	key   string
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// cutUnits removes about excess UTF-16 units from the end of s, never
// splitting a surrogate pair, and re-sanitizes the result.
func cutUnits(s string, excess int) string {
	rs := []rune(s)
	for excess > 0 && len(rs) > 1 {
		if rs[len(rs)-1] > 0xFFFF {
			excess -= 2
		} else {
			excess--
		}
		rs = rs[:len(rs)-1]
	}
	c, _ := SanitizeName(string(rs))
	return c
}

// uniqueFit applies Namer.Unique and keeps the result within 255 UTF-16
// units by trimming the base name (before the extension) instead of the ~N
// tail, repeating Unique if the trimmed name collides again.
func uniqueFit(nm *Namer, name string) string {
	for {
		u := nm.Unique(name)
		excess := utf16Len(u) - 255
		if excess <= 0 {
			return u
		}
		base, ext := name, ""
		if d := strings.LastIndexByte(name, '.'); d > 0 {
			base, ext = name[:d], name[d:]
		}
		if utf16Len(base) > excess {
			name = cutUnits(base, excess) + ext
		} else {
			name = cutUnits(name, excess)
		}
	}
}

func fatTime(t time.Time) (date, tm uint16) {
	if t.IsZero() || t.Year() < 1980 {
		return 1<<5 | 1, 0
	}
	if t.Year() > 2107 {
		return 127<<9 | 12<<5 | 31, 23<<11 | 59<<5 | 29
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	tm = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	return
}

func dirEntry(name [11]byte, attr byte, mod time.Time, size uint32) [32]byte {
	var e [32]byte
	copy(e[:11], name[:])
	e[11] = attr
	d, tm := fatTime(mod)
	binary.LittleEndian.PutUint16(e[14:], tm) // created
	binary.LittleEndian.PutUint16(e[16:], d)
	binary.LittleEndian.PutUint16(e[18:], d)  // accessed
	binary.LittleEndian.PutUint16(e[22:], tm) // modified
	binary.LittleEndian.PutUint16(e[24:], d)
	binary.LittleEndian.PutUint32(e[28:], size)
	return e
}

func setCluster(e []byte, c uint32) {
	binary.LittleEndian.PutUint16(e[20:], uint16(c>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(c))
}

func cleanLabel(l string) string {
	if l == "" {
		l = defaultLabel
	}
	l = strings.ToUpper(l)
	var b []byte
	for _, r := range l {
		if r >= 0x80 || r < 0x20 || strings.ContainsRune(`"*+,./:;<=>?[\]|`, r) {
			r = '_'
		}
		b = append(b, byte(r))
	}
	if len(b) > 11 {
		b = b[:11]
	}
	for len(b) < 11 {
		b = append(b, ' ')
	}
	return string(b)
}

// NewVolume scans src and lays out the disk. The returned warnings cover the
// source's own and every name changed or entry dropped here.
func NewVolume(src Source, opt VolumeOptions) (*Volume, []string, error) {
	root, warns, err := src.Tree()
	if err != nil {
		return nil, warns, err
	}
	if root == nil || !root.IsDir {
		return nil, warns, errors.New("tsshare: source has no root directory")
	}
	if opt.PartStartSector == 0 {
		opt.PartStartSector = defaultPartStart
	}
	label := cleanLabel(opt.Label)
	v := &Volume{src: src, partStart: int64(opt.PartStartSector)}

	// Pass 1: names and directory bytes, breadth first.
	paths := []string{""}
	v.dirs = append(v.dirs, dirExtent{node: root, parent: -1})
	for di := 0; di < len(v.dirs); di++ {
		d := &v.dirs[di]
		dpath := paths[di]
		shown := "/" + dpath
		kids := make([]dirChild, 0, len(d.node.Children))
		for _, c := range d.node.Children {
			if c == nil {
				continue
			}
			if !c.IsDir && (c.Size < 0 || c.Size > maxFileSize) {
				warns = append(warns, fmt.Sprintf("%s%s: skipped, size %d does not fit FAT32", shown, c.Name, c.Size))
				continue
			}
			clean, _ := SanitizeName(c.Name)
			kids = append(kids, dirChild{node: c, clean: clean, key: strings.ToLower(clean)})
		}
		sort.SliceStable(kids, func(i, j int) bool {
			if kids[i].key != kids[j].key {
				return kids[i].key < kids[j].key
			}
			return kids[i].node.Name < kids[j].node.Name
		})
		nm := NewNamer()
		var data []byte
		entries := 0
		if di == 0 {
			e := dirEntry([11]byte([]byte(label)), 0x08, root.ModTime, 0)
			data = append(data, e[:]...)
			entries = 1
		} else {
			dot := dirEntry([11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}, 0x10, d.node.ModTime, 0)
			dd := dirEntry([11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}, 0x10, d.node.ModTime, 0)
			data = append(data, dot[:]...)
			data = append(data, dd[:]...)
			entries = 2
		}
		dropped := 0
		for ki, k := range kids {
			name := uniqueFit(nm, k.clean)
			if name != k.node.Name {
				warns = append(warns, fmt.Sprintf("%s%s: renamed to %q", shown, k.node.Name, name))
			}
			alias := nm.Alias(name)
			lfn := LFNEntries(name, alias)
			if entries+len(lfn)+1 > maxDirEntries {
				dropped = len(kids) - ki
				break
			}
			entries += len(lfn) + 1
			for i := range lfn {
				data = append(data, lfn[i][:]...)
			}
			var e [32]byte
			if k.node.IsDir {
				e = dirEntry(alias, 0x10, k.node.ModTime, 0)
				d.refs = append(d.refs, int32(len(v.dirs)))
				v.dirs = append(v.dirs, dirExtent{node: k.node, parent: int32(di)})
				paths = append(paths, dpath+name+"/")
				d = &v.dirs[di] // the append may have moved the slice
			} else {
				e = dirEntry(alias, 0x21, k.node.ModTime, uint32(k.node.Size))
				d.refs = append(d.refs, ^int32(len(v.files)))
				v.files = append(v.files, fileExtent{size: uint32(k.node.Size), node: k.node})
			}
			data = append(data, e[:]...)
		}
		if dropped > 0 {
			warns = append(warns, fmt.Sprintf("%s: directory limit of %d entries reached, %d entries dropped", shown, maxDirEntries, dropped))
		}
		d.data = data
		d.node = nil // keep the tree reachable only through files
	}

	// Choose the cluster size.
	count := func(spc int) (n uint64) {
		cb := uint64(spc) * sectorSize
		for i := range v.dirs {
			n += max((uint64(len(v.dirs[i].data))+cb-1)/cb, 1)
		}
		for i := range v.files {
			n += (uint64(v.files[i].size) + cb - 1) / cb
		}
		return n
	}
	spc := opt.SectorsPerCluster
	if spc != 0 {
		if spc < 0 || spc > 128 || spc&(spc-1) != 0 {
			return nil, warns, fmt.Errorf("tsshare: bad sectors per cluster %d", spc)
		}
	} else {
		// 4 KiB clusters whenever the content fits (the layout proven on a
		// real card); small sources are padded with free clusters up to
		// minClusters instead of shrinking the cluster size.
		spc = 8
		for spc < 64 && count(spc) > maxClusters {
			spc *= 2
		}
	}
	used := count(spc)
	if used > maxClusters {
		return nil, warns, fmt.Errorf("tsshare: %d clusters exceed the FAT32 limit; content is too large", used)
	}
	total := max(used, minClusters)
	v.spc = spc
	v.totalClusters = uint32(total)
	v.usedEnd = uint32(used) + 2
	v.fatSectors = (int64(total) + 2) * 4 / sectorSize
	if (int64(total)+2)*4%sectorSize != 0 {
		v.fatSectors++
	}
	// Pad the reserved area so the data area starts on a cluster boundary.
	v.reserved = minReserved
	for (v.reserved+2*v.fatSectors)%int64(spc) != 0 {
		v.reserved++
	}
	v.dataStart = v.reserved + 2*v.fatSectors
	v.partSectors = v.dataStart + int64(total)*int64(spc)
	if v.partSectors >= 1<<32 || v.partSectors+v.partStart >= 1<<32 {
		return nil, warns, errors.New("tsshare: volume too large for a 32-bit sector count")
	}
	v.size = (v.partStart*sectorSize + v.partSectors*sectorSize + 1<<20 - 1) &^ (1<<20 - 1)

	// Pass 2: allocate (dirs then files) and patch the cluster fields.
	cb := uint64(spc) * sectorSize
	next := uint32(2)
	for i := range v.dirs {
		d := &v.dirs[i]
		d.start = next
		d.count = uint32(max((uint64(len(d.data))+cb-1)/cb, 1))
		next += d.count
	}
	for i := range v.files {
		f := &v.files[i]
		f.start = next
		f.count = uint32((uint64(f.size) + cb - 1) / cb)
		next += f.count
	}
	for i := range v.dirs {
		d := &v.dirs[i]
		off := 0
		if i == 0 {
			off = 32 // label
		} else {
			setCluster(d.data[0:32], d.start)
			if d.parent > 0 {
				setCluster(d.data[32:64], v.dirs[d.parent].start)
			}
			off = 64
		}
		ri := 0
		for ; off < len(d.data); off += 32 {
			if d.data[off+11] == 0x0F {
				continue
			}
			r := d.refs[ri]
			ri++
			if r >= 0 {
				setCluster(d.data[off:off+32], v.dirs[r].start)
			} else if f := v.files[^r]; f.count > 0 {
				setCluster(d.data[off:off+32], f.start)
			}
		}
		d.refs = nil
	}
	v.buildHeader(label)
	return v, warns, nil
}

func (v *Volume) buildHeader(label string) {
	le := binary.LittleEndian
	// MBR
	mbr := make([]byte, sectorSize)
	le.PutUint32(mbr[440:], 0x54535348) // disk signature
	pe := mbr[446:462]
	pe[0] = 0x00
	pe[1], pe[2], pe[3] = 0xFE, 0xFF, 0xFF
	pe[4] = 0x0C
	pe[5], pe[6], pe[7] = 0xFE, 0xFF, 0xFF
	le.PutUint32(pe[8:], uint32(v.partStart))
	le.PutUint32(pe[12:], uint32(v.partSectors))
	mbr[510], mbr[511] = 0x55, 0xAA
	v.head = mbr

	// reserved area
	r := make([]byte, v.reserved*sectorSize)
	b := r[:sectorSize]
	b[0], b[1], b[2] = 0xEB, 0x58, 0x90
	copy(b[3:11], "TSSHARE ")
	le.PutUint16(b[11:], sectorSize)
	b[13] = byte(v.spc)
	le.PutUint16(b[14:], uint16(v.reserved))
	b[16] = 2
	b[21] = 0xF8
	le.PutUint16(b[24:], 63)
	le.PutUint16(b[26:], 255)
	le.PutUint32(b[28:], uint32(v.partStart))
	le.PutUint32(b[32:], uint32(v.partSectors))
	le.PutUint32(b[36:], uint32(v.fatSectors))
	le.PutUint32(b[44:], 2)
	le.PutUint16(b[48:], 1)
	le.PutUint16(b[50:], 6)
	b[64] = 0x80
	b[66] = 0x29
	le.PutUint32(b[67:], 0x54533231)
	copy(b[71:82], label)
	copy(b[82:90], "FAT32   ")
	b[510], b[511] = 0x55, 0xAA
	fs := r[sectorSize : 2*sectorSize]
	le.PutUint32(fs[0:], 0x41615252)
	le.PutUint32(fs[484:], 0x61417272)
	free := v.totalClusters + 2 - v.usedEnd
	le.PutUint32(fs[488:], free)
	hint := v.usedEnd
	if free == 0 {
		hint = 0xFFFFFFFF
	}
	le.PutUint32(fs[492:], hint)
	le.PutUint32(fs[508:], 0xAA550000)
	copy(r[6*sectorSize:], b)
	copy(r[7*sectorSize:], fs)
	v.rsvd = r
}

// ---------------------------------------------------------------- reading

// extentEnd returns the end (exclusive) of the allocated extent holding
// cluster c, which must be in [2, usedEnd).
func (v *Volume) extentEnd(c uint32) uint32 {
	if i := sort.Search(len(v.dirs), func(i int) bool { return v.dirs[i].start+v.dirs[i].count > c }); i < len(v.dirs) {
		return v.dirs[i].start + v.dirs[i].count
	}
	// Ends are non-decreasing (empty files repeat the previous end), so the
	// first file whose end exceeds c is the one holding it.
	i := sort.Search(len(v.files), func(i int) bool { return v.files[i].start+v.files[i].count > c })
	return v.files[i].start + v.files[i].count
}

// fatSector computes FAT sector k into buf.
func (v *Volume) fatSector(k int64, buf []byte) {
	le := binary.LittleEndian
	first := uint32(k * (sectorSize / 4))
	var end uint32
	for j := uint32(0); j < sectorSize/4; j++ {
		c := first + j
		var val uint32
		switch {
		case c == 0:
			val = 0x0FFFFFF8
		case c == 1:
			val = endOfChain
		case c >= v.usedEnd:
			val = 0
		default:
			if c >= end {
				end = v.extentEnd(c)
			}
			if c == end-1 {
				val = endOfChain
			} else {
				val = c + 1
			}
		}
		le.PutUint32(buf[j*4:], val)
	}
}

func zero(p []byte) {
	for i := range p {
		p[i] = 0
	}
}

// ReadAt fills p completely from the virtual disk. Bytes past the end and in
// unallocated space are zero. The only error is a failing source read.
func (v *Volume) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("tsshare: negative offset")
	}
	total := len(p)
	for len(p) > 0 {
		n, err := v.readPiece(p, off)
		p = p[n:]
		off += int64(n)
		if err != nil {
			return total - len(p), err
		}
	}
	return total, nil
}

// readPiece fills a non-empty prefix of p and reports its length.
func (v *Volume) readPiece(p []byte, off int64) (int, error) {
	want := int64(len(p))
	partOff := v.partStart * sectorSize
	switch {
	case off >= v.size:
		zero(p)
		return len(p), nil
	case off < sectorSize:
		n := min(want, sectorSize-off)
		copy(p[:n], v.head[off:])
		return int(n), nil
	case off < partOff:
		n := min(want, partOff-off)
		zero(p[:n])
		return int(n), nil
	}
	po := off - partOff
	rsvdBytes := v.reserved * sectorSize
	fatBytes := v.fatSectors * sectorSize
	dataOff := v.dataStart * sectorSize
	switch {
	case po >= v.partSectors*sectorSize:
		zero(p)
		return len(p), nil
	case po < rsvdBytes:
		n := min(want, rsvdBytes-po)
		copy(p[:n], v.rsvd[po:])
		return int(n), nil
	case po < dataOff:
		fo := (po - rsvdBytes) % fatBytes
		k := fo / sectorSize
		var buf [sectorSize]byte
		v.fatSector(k, buf[:])
		n := min(want, sectorSize-fo%sectorSize)
		copy(p[:n], buf[fo%sectorSize:])
		return int(n), nil
	}
	cb := int64(v.spc) * sectorSize
	do := po - dataOff
	c := uint32(do/cb) + 2
	within := do % cb
	if c >= v.usedEnd {
		zero(p)
		return len(p), nil
	}
	if i := sort.Search(len(v.dirs), func(i int) bool { return v.dirs[i].start+v.dirs[i].count > c }); i < len(v.dirs) {
		d := &v.dirs[i]
		eo := int64(c-d.start)*cb + within
		extLen := int64(d.count) * cb
		n := min(want, extLen-eo)
		if eo >= int64(len(d.data)) {
			zero(p[:n])
		} else {
			k := copy(p[:n], d.data[eo:])
			zero(p[k:n])
		}
		return int(n), nil
	}
	i := sort.Search(len(v.files), func(i int) bool { return v.files[i].start+v.files[i].count > c })
	f := &v.files[i]
	eo := int64(c-f.start)*cb + within
	extLen := int64(f.count) * cb
	n := min(want, extLen-eo)
	if eo >= int64(f.size) {
		zero(p[:n])
		return int(n), nil
	}
	m := min(n, int64(f.size)-eo)
	got, err := v.src.ReadFileAt(f.node, p[:m], eo)
	for got < int(m) && err == nil {
		var k int
		k, err = v.src.ReadFileAt(f.node, p[got:m], eo+int64(got))
		if k == 0 && err == nil {
			err = io.ErrUnexpectedEOF
		}
		got += k
	}
	if got < int(m) {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return got, fmt.Errorf("tsshare: reading %q at %d: %w", f.node.Name, eo, err)
	}
	zero(p[m:n])
	return int(n), nil
}

// Geometry summarizes the layout of a Volume for the startup report.
type Geometry struct {
	SectorsPerCluster int
	Clusters          uint32 // clusters of the data area
	FATSectors        int64
	PartStart         int64 // sector
	Dirs, Files       int   // including the root directory
	DiskBytes         int64
}

// Geometry reports the volume layout.
func (v *Volume) Geometry() Geometry {
	return Geometry{v.spc, v.totalClusters, v.fatSectors, v.partStart, len(v.dirs), len(v.files), v.size}
}
