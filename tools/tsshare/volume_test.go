package tsshare

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
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
	bpb := part[:512]
	le := binary.LittleEndian
	if bpb[510] != 0x55 || bpb[511] != 0xAA {
		t.Error("BPB signature")
	}
	if le.Uint16(bpb[11:]) != 512 || le.Uint16(bpb[14:]) != 32 || bpb[16] != 2 || bpb[21] != 0xF8 {
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
	fat1 := readAll(t, v, (128+32)*512, int(fatSz*512))
	fat2 := readAll(t, v, (128+32+fatSz)*512, int(fatSz*512))
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
	total := int(psec-32-2*uint32(fatSz)) / int(bpb[13])
	for c := 2; c < total+2; c++ {
		if le.Uint32(fat1[c*4:])&0x0FFFFFFF == 0 {
			free++
		}
	}
	if le.Uint32(fs[488:]) != uint32(free) {
		t.Errorf("FSInfo free %d, FAT free %d", le.Uint32(fs[488:]), free)
	}
	// root directory: first entry is the volume label
	dataStart := int64(128+32) + 2*fatSz
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
	if v.spc != 1 || clusters(v) != 65525 {
		t.Errorf("tiny tree: spc=%d clusters=%d", v.spc, clusters(v))
	}
	if v.Size() > 40<<20 {
		t.Errorf("tiny volume is %d bytes", v.Size())
	}
	for _, c := range []struct {
		size int64
		spc  int
	}{{100 << 20, 2}, {50 << 20, 1}, {150 << 20, 4}, {300 << 20, 8}, {20 << 30, 8}} {
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
