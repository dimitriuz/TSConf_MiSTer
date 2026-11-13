package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

var t0 = time.Date(2020, 5, 17, 13, 45, 30, 0, time.UTC)

func smallFixture() map[string]memFile {
	m := func(size int64) memFile { return memFile{Size: size, ModTime: t0} }
	return map[string]memFile{
		"HELLO.TXT":            {Data: []byte("hello, world\n"), ModTime: t0},
		"a/b/c/deep.bin":       m(5000),
		"a/b/c/zero.dat":       m(0),
		"a/one cluster.bin":    m(512),
		"a/cluster plus 1.bin": m(513),
		"empty dir/":           {ModTime: t0},
		"Музыка/Песня №1.vgm":  m(12345),
		"Emoji \U0001F600.txt": m(77),
		"Case.txt":             m(10),
		"CASE.TXT":             m(11),
		"Long name with spaces and a very long tail xxxxxxxxxxxxxxxxxxxxxxxx.tap": m(3000),
	}
}

func buildSmall(t *testing.T, opt VolumeOptions) (*Volume, *memSource) {
	t.Helper()
	src := newMemSource(smallFixture())
	v, w, err := NewVolume(src, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("warnings: %q", w)
	return v, src
}

func TestMBRAndBootSector(t *testing.T) {
	v, _ := buildSmall(t, VolumeOptions{Label: "myshare"})
	if v.Size()%(1<<20) != 0 {
		t.Errorf("Size %d not a multiple of 1 MiB", v.Size())
	}
	mbr := readAll(t, v, 0, 512)
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		t.Error("MBR signature")
	}
	pe := mbr[446:462]
	if pe[4] != 0x0C {
		t.Errorf("partition type %#x", pe[4])
	}
	if binary.LittleEndian.Uint32(pe[8:]) != 128 {
		t.Errorf("start %d", binary.LittleEndian.Uint32(pe[8:]))
	}
	psec := binary.LittleEndian.Uint32(pe[12:])
	if int64(128+psec)*512 > v.Size() {
		t.Errorf("partition exceeds disk")
	}
	part := readAll(t, v, 128*512, 32*512)
	rsv := int64(v.reserved)
	bpb := part[:512]
	le := binary.LittleEndian
	if bpb[510] != 0x55 || bpb[511] != 0xAA {
		t.Error("BPB signature")
	}
	if le.Uint16(bpb[11:]) != 512 || int64(le.Uint16(bpb[14:])) != rsv || bpb[16] != 2 || bpb[21] != 0xF8 {
		t.Errorf("BPB basics wrong: % x", bpb[:64])
	}
	if le.Uint32(bpb[28:]) != 128 || le.Uint32(bpb[32:]) != psec || le.Uint32(bpb[44:]) != 2 ||
		le.Uint16(bpb[48:]) != 1 || le.Uint16(bpb[50:]) != 6 {
		t.Errorf("BPB fields wrong")
	}
	if string(bpb[71:82]) != "MYSHARE    " {
		t.Errorf("label %q", bpb[71:82])
	}
	if !bytes.Equal(part[6*512:7*512], bpb) {
		t.Error("backup BPB differs")
	}
	fs := part[512:1024]
	if le.Uint32(fs[0:]) != 0x41615252 || le.Uint32(fs[484:]) != 0x61417272 || le.Uint32(fs[508:]) != 0xAA550000 {
		t.Error("FSInfo signatures")
	}
	if !bytes.Equal(part[7*512:8*512], fs) {
		t.Error("backup FSInfo differs")
	}
	fatSz := int64(le.Uint32(bpb[36:]))
	fat1 := readAll(t, v, (128+rsv)*512, int(fatSz*512))
	fat2 := readAll(t, v, (128+rsv+fatSz)*512, int(fatSz*512))
	if !bytes.Equal(fat1, fat2) {
		t.Error("FATs differ")
	}
	if le.Uint32(fat1[0:]) != 0x0FFFFFF8 || le.Uint32(fat1[4:]) != 0x0FFFFFFF {
		t.Error("FAT entries 0/1")
	}
	// free count in FSInfo equals the zero entries counted in the FAT
	nclus := int(bpb[13])
	_ = nclus
	free := 0
	total := int(psec-uint32(rsv)-2*uint32(fatSz)) / int(bpb[13])
	for c := 2; c < total+2; c++ {
		if le.Uint32(fat1[c*4:])&0x0FFFFFFF == 0 {
			free++
		}
	}
	if le.Uint32(fs[488:]) != uint32(free) {
		t.Errorf("FSInfo free %d, FAT free %d", le.Uint32(fs[488:]), free)
	}
	// root directory: first entry is the volume label
	dataStart := 128 + rsv + 2*fatSz
	root := readAll(t, v, dataStart*512, 32)
	if string(root[:11]) != "MYSHARE    " || root[11] != 0x08 {
		t.Errorf("root label entry % x", root)
	}
	// default label
	v2, _, err := NewVolume(newMemSource(smallFixture()), VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(readAll(t, v2, 128*512+71, 11)) != "TSSHARE    " {
		t.Error("default label")
	}
}

func TestSmallTree(t *testing.T) {
	needTool(t, "fsck.vfat", "mdir", "mcopy")
	v, src := buildSmall(t, VolumeOptions{})
	img := dumpVolume(t, v)
	out := fsck(t, img, v)
	t.Logf("fsck.vfat -n:\n%s", out)

	// directory listing equals the source after sanitizing / case-twin renaming
	// mtools only knows UCS-2, so it shows the emoji (a surrogate pair) as
	// "__"; the pair itself is checked in the raw directory bytes below.
	want := []string{
		"CASE.TXT", "Case~1.txt", "Emoji __.txt", "HELLO.TXT",
		"Long name with spaces and a very long tail xxxxxxxxxxxxxxxxxxxxxxxx.tap",
		"a/", "a/b/", "a/b/c/", "a/b/c/deep.bin", "a/b/c/zero.dat",
		"a/cluster plus 1.bin", "a/one cluster.bin", "empty dir/",
		"Музыка/", "Музыка/Песня №1.vgm",
	}
	sort.Strings(want)
	got := mdirList(t, img)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("listing:\n got %q\nwant %q", got, want)
	}

	// the emoji's surrogate pair is in the root directory's LFN entries
	rootDir := readAll(t, v, (v.partStart+v.dataStart)*512, 4096)
	if !bytes.Contains(rootDir, []byte{0x3D, 0xD8, 0x00, 0xDE}) {
		t.Error("emoji surrogate pair missing from root directory")
	}

	// file contents
	for p, f := range smallFixture() {
		if strings.HasSuffix(p, "/") {
			continue
		}
		name := p
		if p == "Case.txt" {
			name = "Case~1.txt"
		}
		if p == "Emoji \U0001F600.txt" {
			name = "EMOJI_~1.TXT" // mtools cannot match the astral name
		}
		b := mcopyOut(t, img, "::"+name)
		size := f.Size
		if f.Data != nil {
			size = int64(len(f.Data))
		}
		if int64(len(b)) != size {
			t.Errorf("%s: size %d, want %d", p, len(b), size)
			continue
		}
		if !bytes.Equal(b, src.expected(p, 0, len(b))) {
			t.Errorf("%s: contents differ", p)
		}
	}
}

// bigSource is a tree of total size bytes in files of at most 1 GiB.
func bigSource(size int64) *memSource {
	m := map[string]memFile{}
	for i := 0; size > 0; i++ {
		n := min(size, 1<<30)
		m[fmt.Sprintf("big%03d.bin", i)] = memFile{Size: n, ModTime: t0}
		size -= n
	}
	return newMemSource(m)
}

func TestSectorsPerCluster(t *testing.T) {
	clusters := func(v *Volume) int { return int(v.totalClusters) }
	v, _ := buildSmall(t, VolumeOptions{})
	// 4 KiB clusters padded with free clusters to the FAT32 minimum:
	// 512 FAT sectors, reserved 32 (32+2*512 is a multiple of 8), 65,525
	// clusters of 8 sectors, a 257 MiB virtual disk.
	if v.spc != 8 || clusters(v) != 65525 || v.fatSectors != 512 || v.reserved != 32 {
		t.Errorf("tiny tree: spc=%d clusters=%d fat=%d reserved=%d", v.spc, clusters(v), v.fatSectors, v.reserved)
	}
	if v.Size() != 257<<20 {
		t.Errorf("tiny volume is %d bytes, want 257 MiB", v.Size())
	}
	if v.dataStart%int64(v.spc) != 0 {
		t.Errorf("data area start %d not cluster aligned", v.dataStart)
	}
	for _, c := range []struct {
		size int64
		spc  int
	}{{1 << 20, 8}, {50 << 20, 8}, {100 << 20, 8}, {150 << 20, 8}, {300 << 20, 8}, {20 << 30, 8}, {1536 << 30, 16}} {
		v, _, err := NewVolume(bigSource(c.size), VolumeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if v.spc != c.spc {
			t.Errorf("%d MiB: spc=%d want %d", c.size>>20, v.spc, c.spc)
		}
		if clusters(v) < 65525 {
			t.Errorf("%d MiB: only %d clusters", c.size>>20, clusters(v))
		}
		if v.Size()%(1<<20) != 0 {
			t.Errorf("size %d not 1MiB multiple", v.Size())
		}
		if v.dataStart%int64(v.spc) != 0 || v.reserved < 32 || v.reserved >= 32+int64(v.spc) {
			t.Errorf("%d MiB: reserved=%d dataStart=%d not aligned to %d", c.size>>20, v.reserved, v.dataStart, v.spc)
		}
	}
	v, _, err := NewVolume(bigSource(300<<20), VolumeOptions{SectorsPerCluster: 16})
	if err != nil || v.spc != 16 {
		t.Errorf("forced: %v spc=%v", err, v)
	}
}

func TestReadAtAnyRange(t *testing.T) {
	needTool(t)
	v, _ := buildSmall(t, VolumeOptions{})
	img := dumpVolume(t, v)
	disk, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(disk)) != v.Size() {
		t.Fatalf("dump %d != Size %d", len(disk), v.Size())
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		off := rng.Int63n(v.Size() + 100000)
		if i%3 == 0 { // concentrate on the metadata and early data
			off = rng.Int63n(1 << 20)
		}
		n := 1 + rng.Intn(40000)
		got := readAll(t, v, off, n)
		want := make([]byte, n)
		if off < int64(len(disk)) {
			copy(want, disk[off:])
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("mismatch at off=%d n=%d", off, n)
		}
	}
	past := readAll(t, v, v.Size()+5, 1000)
	if !bytes.Equal(past, make([]byte, 1000)) {
		t.Error("past end not zero")
	}
}

func TestDirectoryLimit(t *testing.T) {
	needTool(t, "fsck.vfat", "dd")
	files := map[string]memFile{}
	for i := 0; i < 70000; i++ {
		files[fmt.Sprintf("F%07d.TXT", i)] = memFile{ModTime: t0}
	}
	files["x/keep.txt"] = memFile{Size: 5, ModTime: t0}
	v, w, err := NewVolume(newMemSource(files), VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range w {
		if strings.Contains(s, "limit") {
			found = true
		}
	}
	if !found {
		t.Errorf("no limit warning in %d warnings: %.200q", len(w), w)
	}
	img := dumpVolume(t, v)
	fsck(t, img, v)
	names := mdirList(t, img)
	// root: 1 label + 65535 entries; "x" sorts after "F..." names so it is dropped
	if len(names) != 65535 {
		t.Errorf("listed %d entries, want 65535", len(names))
	}
}

func TestSourceError(t *testing.T) {
	src := newMemSource(map[string]memFile{
		"a.bin": {Size: 20000, ModTime: t0},
		"b.bin": {Size: 20000, ModTime: t0},
		"c.bin": {Size: 20000, ModTime: t0},
	})
	src.failPath = "b.bin"
	v, _, err := NewVolume(src, VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var bf *fileExtent
	for i := range v.files {
		if src.paths[v.files[i].node.ID] == "b.bin" {
			bf = &v.files[i]
		}
	}
	cb := int64(v.spc) * 512
	base := v.dataStart*512 + (int64(bf.start)-2)*cb + 128*512
	buf := make([]byte, 100)
	if _, err := v.ReadAt(buf, base+50); err == nil {
		t.Error("expected error inside failing file")
	}
	for _, f := range v.files {
		if f.node == bf.node {
			continue
		}
		b := v.dataStart*512 + (int64(f.start)-2)*cb + 128*512
		if _, err := v.ReadAt(buf, b+50); err != nil {
			t.Errorf("other file: %v", err)
		}
	}
	// metadata reads are fine
	if _, err := v.ReadAt(buf, 0); err != nil {
		t.Error(err)
	}
}

func TestLongNamesAndTwins(t *testing.T) {
	needTool(t, "fsck.vfat", "mdir", "dd")
	long := strings.Repeat("a", 251) + ".txt" // 255 units
	files := map[string]memFile{
		long:                              {Size: 10, ModTime: t0},
		strings.ToUpper(long):             {Size: 11, ModTime: t0},
		strings.Repeat("b", 300) + ".dat": {Size: 12, ModTime: t0},
		"ends with dot. ":                 {Size: 1, ModTime: t0},
		"bad:name?.txt":                   {Size: 1, ModTime: t0},
	}
	v, w, err := NewVolume(newMemSource(files), VolumeOptions{SectorsPerCluster: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("warnings: %q", w)
	img := dumpVolume(t, v)
	t.Logf("%s", fsck(t, img, v))
	names := mdirList(t, img)
	if len(names) != 5 {
		t.Fatalf("got %q", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if len(utf16Units(n)) > 255 {
			t.Errorf("name over 255 units: %.40q", n)
		}
		if seen[strings.ToLower(n)] {
			t.Errorf("case twin %.40q", n)
		}
		seen[strings.ToLower(n)] = true
	}
}

func utf16Units(s string) []uint16 { return utf16.Encode([]rune(s)) }

// fixedSource serves a pre-built tree so a test can measure what NewVolume
// itself retains.
type fixedSource struct {
	*memSource
	root *Node
}

func (f fixedSource) Tree() (*Node, []string, error) { return f.root, nil, nil }

func TestScaleMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	root := &Node{IsDir: true}
	nodes := 0
	for d1 := 0; d1 < 33; d1++ {
		a := &Node{Name: fmt.Sprintf("Collection %02d", d1), IsDir: true}
		root.Children = append(root.Children, a)
		for d2 := 0; d2 < 31; d2++ {
			b := &Node{Name: fmt.Sprintf("Sub collection %02d", d2), IsDir: true}
			a.Children = append(a.Children, b)
			for f := 0; f < 55; f++ {
				b.Children = append(b.Children, &Node{
					Name: fmt.Sprintf("Some game title %02d.%02d (1991)(Publisher)[a%d].trd", d2, f, f), Size: int64(10000 + f*1000), ModTime: t0, ID: nodes})
				nodes++
			}
		}
	}
	total := nodes + 33*31 + 33
	src := fixedSource{newMemSource(nil), root}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	start := time.Now()
	v, _, err := NewVolume(src, VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	per := (int64(m1.HeapAlloc) - int64(m0.HeapAlloc)) / int64(total)
	t.Logf("%d nodes, build %v, retained %d bytes/node, spc=%d, size=%d MiB", total, el, per, v.spc, v.Size()>>20)
	if per > 300 {
		t.Errorf("retained %d bytes per node", per)
	}
	runtime.KeepAlive(v)
}

func TestGeometry(t *testing.T) {
	v, _, err := NewVolume(newMemSource(map[string]memFile{"a.txt": {Size: 10}, "d/": {}}), VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g := v.Geometry()
	if g.Files != 1 || g.Dirs != 2 || g.DiskBytes != v.Size() || g.PartStart != 128 || g.SectorsPerCluster == 0 {
		t.Fatalf("geometry %+v", g)
	}
}

// A tree of one root-level file whose cluster count at 4 KiB clusters, plus
// the root directory's single cluster, is exactly total.
func TestClusterCountBoundary4K(t *testing.T) {
	for _, c := range []struct {
		total    int64 // clusters of 4 KiB the content needs (root dir + file)
		spc      int
		clusters uint32
	}{
		// one short of FAT32's minimum: padded with a free cluster
		{65524, 8, 65525},
		{65525, 8, 65525},
		{65526, 8, 65526},
	} {
		src := newMemSource(map[string]memFile{"big.bin": {Size: (c.total - 1) * 4096, ModTime: t0}})
		v, _, err := NewVolume(src, VolumeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if v.spc != c.spc || v.totalClusters != c.clusters {
			t.Errorf("content of %d 4K clusters: spc=%d clusters=%d, want spc=%d clusters=%d",
				c.total, v.spc, v.totalClusters, c.spc, c.clusters)
		}
		if v.usedEnd != uint32(2+c.total) {
			t.Errorf("usedEnd %d", v.usedEnd)
		}
	}
	// One byte more than 65,525 clusters rolls the file into another cluster.
	src := newMemSource(map[string]memFile{"big.bin": {Size: 65524*4096 + 1, ModTime: t0}})
	v, _, err := NewVolume(src, VolumeOptions{})
	if err != nil || v.spc != 8 || v.totalClusters != 65526 {
		t.Errorf("65525 clusters + 1 byte: %v spc=%d clusters=%d", err, v.spc, v.totalClusters)
	}
}

// listing of a directory image under the root, as a set of names
func nameSet(l []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range l {
		m[n] = true
	}
	return m
}

// Directories whose entries end exactly at a cluster boundary (no end marker
// fits) and one entry beyond it. Names are upper-case 8.3, so each takes one
// entry.
func TestDirectoryClusterBoundary(t *testing.T) {
	needTool(t, "fsck.vfat", "mdir", "dd")
	for _, spc := range []int{1, 2} {
		perCluster := 16 * spc // 32-byte entries per cluster
		for _, extra := range []int{-1, 0, 1} {
			// The directory holds `perCluster+extra` entries: the root has the
			// label and the subdirectory "DIR" besides its k files; "DIR" has "."
			// and ".." besides its k files. All names are short (no LFN entries).
			k := perCluster - 2 + extra
			for _, root := range []bool{false, true} {
				prefix := "DIR/"
				if root {
					prefix = ""
				}
				files := map[string]memFile{}
				want := map[string]bool{"DIR/": true}
				for i := 0; i < k; i++ {
					n := fmt.Sprintf("F%03d.TXT", i)
					files[prefix+n] = memFile{Size: int64(10 + i), ModTime: t0}
					want[prefix+n] = true
				}
				if root {
					files["DIR/X.TXT"] = memFile{Size: 1, ModTime: t0} // "DIR" exists in both variants
					want["DIR/X.TXT"] = true
				}
				v, w, err := NewVolume(newMemSource(files), VolumeOptions{SectorsPerCluster: spc})
				if err != nil {
					t.Fatal(err)
				}
				if len(w) != 0 {
					t.Errorf("warnings: %q", w)
				}
				name := fmt.Sprintf("spc=%d root=%v entries=%d", spc, root, perCluster+extra)
				idx := 1 // "DIR"
				if root {
					idx = 0
				}
				wantClusters := (perCluster + extra + perCluster - 1) / perCluster
				if got := int(v.dirs[idx].count); got != wantClusters {
					t.Errorf("%s: directory has %d clusters, want %d", name, got, wantClusters)
				}
				img := dumpVolume(t, v)
				fsck(t, img, v)
				got := nameSet(mdirList(t, img))
				if len(got) != len(want) {
					t.Errorf("%s: listed %d names, want %d", name, len(got), len(want))
				}
				for n := range want {
					if !got[n] {
						t.Errorf("%s: %s missing from listing", name, n)
					}
				}
			}
		}
	}
}

// A file whose size is a whole number of clusters, and one byte more or less.
func TestFileClusterBoundary(t *testing.T) {
	needTool(t, "fsck.vfat", "mcopy", "dd")
	for _, spc := range []int{1, 2} {
		cb := int64(spc) * 512
		files := map[string]memFile{}
		for _, sz := range []int64{cb - 1, cb, cb + 1, 3 * cb, 3*cb + 1, 8*cb - 1, 8 * cb} {
			files[fmt.Sprintf("S%d.BIN", sz)] = memFile{Size: sz, ModTime: t0}
		}
		src := newMemSource(files)
		v, _, err := NewVolume(src, VolumeOptions{SectorsPerCluster: spc})
		if err != nil {
			t.Fatal(err)
		}
		img := dumpVolume(t, v)
		fsck(t, img, v)
		for _, f := range v.files {
			p := src.paths[f.node.ID]
			wantClusters := uint32((int64(f.size) + cb - 1) / cb)
			if f.count != wantClusters {
				t.Errorf("spc=%d %s: %d clusters, want %d", spc, p, f.count, wantClusters)
			}
			b := mcopyOut(t, img, "::"+p)
			if int64(len(b)) != int64(f.size) || !bytes.Equal(b, src.expected(p, 0, len(b))) {
				t.Errorf("spc=%d %s: mcopy gave %d bytes (want %d) or wrong contents", spc, p, len(b), f.size)
			}
		}
	}
}

// rootEntry returns the 32-byte short directory entry of a file in the root.
func rootEntry(t *testing.T, v *Volume, short string) []byte {
	t.Helper()
	dir := readAll(t, v, (v.partStart+v.dataStart)*sectorSize, int(v.dirs[0].count)*v.spc*sectorSize)
	want := []byte(fmt.Sprintf("%-8s%-3s", strings.TrimSuffix(short, path.Ext(short)), strings.TrimPrefix(path.Ext(short), ".")))
	for off := 0; off+32 <= len(dir); off += 32 {
		if dir[off+11] != 0x0F && bytes.Equal(dir[off:off+11], want) {
			return dir[off : off+32]
		}
	}
	t.Fatalf("no root entry %q", short)
	return nil
}

func TestFATTimestamps(t *testing.T) {
	cases := []struct {
		name       string
		mod        time.Time
		y, mo, d   int
		h, mi, sec int
	}{
		{"E1975.TXT", time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC), 1980, 1, 1, 0, 0, 0},
		{"E1979.TXT", time.Date(1979, 12, 31, 23, 59, 59, 0, time.UTC), 1980, 1, 1, 0, 0, 0},
		{"E1980.TXT", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), 1980, 1, 1, 0, 0, 0},
		{"E1980B.TXT", time.Date(1980, 1, 1, 0, 0, 1, 0, time.UTC), 1980, 1, 1, 0, 0, 0},
		{"E2108.TXT", time.Date(2108, 5, 5, 12, 0, 0, 0, time.UTC), 2107, 12, 31, 23, 59, 58},
		{"E2107.TXT", time.Date(2107, 12, 31, 23, 59, 59, 0, time.UTC), 2107, 12, 31, 23, 59, 58},
		{"ODD.TXT", time.Date(2020, 5, 17, 13, 45, 31, 0, time.UTC), 2020, 5, 17, 13, 45, 30},
		{"EVEN.TXT", time.Date(2020, 5, 17, 13, 45, 30, 0, time.UTC), 2020, 5, 17, 13, 45, 30},
		{"SEC59.TXT", time.Date(2001, 2, 3, 4, 5, 59, 0, time.UTC), 2001, 2, 3, 4, 5, 58},
		{"ZERO.TXT", time.Time{}, 1980, 1, 1, 0, 0, 0},
	}
	files := map[string]memFile{}
	for _, c := range cases {
		files[c.name] = memFile{Size: 5, ModTime: c.mod}
	}
	v, w, err := NewVolume(newMemSource(files), VolumeOptions{})
	if err != nil || len(w) != 0 {
		t.Fatalf("%v %q", err, w)
	}
	le := binary.LittleEndian
	for _, c := range cases {
		e := rootEntry(t, v, c.name)
		// created (16 date, 14 time), accessed (18, date only) and modified
		// (24 date, 22 time) must all carry the expected stamp
		for _, off := range []int{16, 18, 24} {
			date := le.Uint16(e[off:])
			if y, mo, d := int(date>>9)+1980, int(date>>5&15), int(date&31); y != c.y || mo != c.mo || d != c.d {
				t.Errorf("%s date at %d: %d-%02d-%02d, want %d-%02d-%02d", c.name, off, y, mo, d, c.y, c.mo, c.d)
			}
		}
		for _, off := range []int{14, 22} {
			tm := le.Uint16(e[off:])
			if h, mi, sec := int(tm>>11), int(tm>>5&63), int(tm&31)*2; h != c.h || mi != c.mi || sec != c.sec {
				t.Errorf("%s time at %d: %02d:%02d:%02d, want %02d:%02d:%02d", c.name, off, h, mi, sec, c.h, c.mi, c.sec)
			}
		}
	}
}

// A subdirectory over the 65,536-entry limit keeps what fits ("." and ".."
// take two entries) and warns with its path; the volume stays consistent.
func TestSubdirectoryLimit(t *testing.T) {
	needTool(t, "fsck.vfat", "mdir", "dd")
	files := map[string]memFile{"top.txt": {Size: 3, ModTime: t0}}
	for i := 0; i < 70000; i++ {
		files[fmt.Sprintf("sub/F%07d.TXT", i)] = memFile{ModTime: t0}
	}
	files["sub/z/inner.txt"] = memFile{Size: 5, ModTime: t0} // sorts after F..., so it is dropped
	v, w, err := NewVolume(newMemSource(files), VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var limit []string
	for _, s := range w {
		if strings.Contains(s, "limit") {
			limit = append(limit, s)
		}
	}
	if len(limit) != 1 || !strings.Contains(limit[0], "/sub") || !strings.Contains(limit[0], "4467") {
		t.Errorf("limit warnings: %q (want one for /sub dropping 4467 entries)", limit)
	}
	img := dumpVolume(t, v)
	fsck(t, img, v)
	names := mdirList(t, img)
	// top.txt, sub, and 65,534 children of sub
	if len(names) != 2+65534 {
		t.Errorf("listed %d entries, want %d", len(names), 2+65534)
	}
	sub := 0
	for _, n := range names {
		if strings.HasPrefix(n, "sub/") && n != "sub/" {
			sub++
			if strings.HasPrefix(n, "sub/z") {
				t.Errorf("%s should have been dropped", n)
			}
		}
	}
	if sub != 65534 {
		t.Errorf("sub has %d children, want 65534", sub)
	}
}

// A tiny source is padded with free clusters to 65,525 of them; the padding
// is virtual: reads stay fast and allocate nothing proportional to it.
func TestTinyVolumePaddingIsVirtual(t *testing.T) {
	v, _, err := NewVolume(newMemSource(map[string]memFile{}), VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.spc != 8 || v.totalClusters != 65525 || v.fatSectors != 512 || v.Size() != 257<<20 {
		t.Fatalf("empty volume: spc=%d clusters=%d fat=%d size=%d", v.spc, v.totalClusters, v.fatSectors, v.Size())
	}
	buf := make([]byte, 1<<20)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	for off := int64(0); off < v.Size(); off += 1 << 20 {
		if _, err := v.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&m1)
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("reading the whole padded disk took %v", el)
	}
	if grown := m1.TotalAlloc - m0.TotalAlloc; grown > 8<<20 {
		t.Errorf("reading allocated %d bytes", grown)
	}
}

// The data area starts on a cluster boundary for every cluster size and for
// FAT sizes of every residue.
func TestDataAreaAligned(t *testing.T) {
	for _, spc := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
		for _, n := range []int{0, 1, 77, 5000} {
			m := map[string]memFile{}
			for i := 0; i < n; i++ {
				m[fmt.Sprintf("F%05d.BIN", i)] = memFile{Size: 100000, ModTime: t0}
			}
			v, _, err := NewVolume(newMemSource(m), VolumeOptions{SectorsPerCluster: spc})
			if err != nil {
				t.Fatal(err)
			}
			if v.dataStart%int64(spc) != 0 || v.reserved < 32 || v.reserved >= 32+int64(spc) {
				t.Errorf("spc=%d n=%d: reserved=%d fat=%d dataStart=%d", spc, n, v.reserved, v.fatSectors, v.dataStart)
			}
			if v.rsvd == nil || int64(len(v.rsvd)) != v.reserved*512 {
				t.Errorf("reserved area length %d", len(v.rsvd))
			}
		}
	}
}

func TestCheckEntryLimit(t *testing.T) {
	for _, c := range []struct {
		n, max int
		ok     bool
	}{{10, 0, true}, {1 << 30, 0, true}, {10, 10, true}, {11, 10, false}, {250001, 250000, false}} {
		err := checkEntryLimit(c.n, c.max)
		if (err == nil) != c.ok {
			t.Errorf("n=%d max=%d: %v", c.n, c.max, err)
		}
		if err != nil && (!strings.Contains(err.Error(), fmt.Sprint(c.n)) || !strings.Contains(err.Error(), "-max-entries")) {
			t.Errorf("message lacks count or flag: %v", err)
		}
	}
	src := newMemSource(smallFixture())
	if _, _, err := NewVolume(src, VolumeOptions{MaxEntries: 5}); err == nil {
		t.Error("NewVolume accepted a tree over the limit")
	}
	if _, _, err := NewVolume(newMemSource(smallFixture()), VolumeOptions{MaxEntries: 100}); err != nil {
		t.Errorf("under the limit: %v", err)
	}
	if _, _, err := NewVolume(newMemSource(smallFixture()), VolumeOptions{}); err != nil {
		t.Errorf("no limit: %v", err)
	}
}
