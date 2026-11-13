package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// zipSource serves the files of a zip archive.
//
// Time zones: Node.ModTime is built as time.Date(date and clock fields of
// Modified as archive/zip returns them, time.UTC), so Volume writes the
// entry's stored DOS wall-clock fields; an extended timestamp is ignored.
type zipSource struct {
	zpath  string
	f      *os.File
	zr     *zip.Reader
	files  []*zip.File // by Node.ID
	offs   []atomic.Int64
	cache  *byteCache
	budget int64

	inflations atomic.Int64 // whole-entry inflations done (for tests)

	smu     sync.Mutex
	streams map[int]*zipStream
}

// NewZipSource opens the archive and keeps it open until Close.
func NewZipSource(zpath string, o SourceOptions) (Source, error) {
	o = o.withDefaults()
	f, err := os.Open(zpath)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("tsshare: %s: %w", zpath, err)
	}
	budget := int64(o.CacheMB) << 20
	return &zipSource{zpath: zpath, f: f, zr: zr, budget: budget,
		cache: newByteCache(budget), streams: map[int]*zipStream{}}, nil
}

// OpenSource opens a directory or a zip file (verified by reading it).
func OpenSource(p string, o SourceOptions) (Source, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	switch {
	case st.IsDir():
		return NewDirSource(p, o)
	case st.Mode().IsRegular():
		return NewZipSource(p, o)
	}
	return nil, fmt.Errorf("tsshare: %s is neither a directory nor a zip file", p)
}

const cp437High = "ÇüéâäàåçêëèïîìÄÅ" +
	"ÉæÆôöòûùÿÖÜ¢£¥₧ƒ" +
	"áíóúñÑªº¿⌐¬½¼¡«»" +
	"░▒▓│┤╡╢╖╕╣║╗╝╜╛┐" +
	"└┴┬├─┼╞╟╚╔╩╦╠═╬╧" +
	"╨╤╥╙╘╒╓╫╪┘┌█▄▌▐▀" +
	"αßΓπΣσµτΦΘΩδ∞φε∩" +
	"≡±≥≤⌠⌡÷≈°∙·√ⁿ²■ "

func decodeCP437(s string) string {
	hi := []rune(cp437High)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x80 {
			b.WriteByte(c)
		} else {
			b.WriteRune(hi[c-0x80])
		}
	}
	return b.String()
}

func zipTime(t time.Time) time.Time {
	// Keep the fields as archive/zip returns them: the DOS wall clock (in a
	// fixed zone when an extended timestamp is also present). No conversion.
	y, mo, d := t.Date()
	h, mi, sec := t.Clock()
	if t.IsZero() || y < 1980 { // an all-zero DOS time decodes to 1979-11-30
		return time.Time{}
	}
	return time.Date(y, mo, d, h, mi, sec, 0, time.UTC)
}

func (s *zipSource) Tree() (*Node, []string, error) {
	var warns []string
	warn := func(f string, a ...any) {
		warns = append(warns, s.zpath+": "+fmt.Sprintf(f, a...))
	}
	s.files = nil
	root := &Node{IsDir: true}
	nodes := map[string]*Node{"": root}
	var getDir func(p string) *Node
	getDir = func(p string) *Node {
		if n, ok := nodes[p]; ok {
			if !n.IsDir {
				return nil
			}
			return n
		}
		parent := getDir(dir0(p))
		if parent == nil {
			return nil
		}
		n := &Node{Name: p[strings.LastIndexByte(p, '/')+1:], IsDir: true}
		parent.Children = append(parent.Children, n)
		nodes[p] = n
		return n
	}
	for _, zf := range s.zr.File {
		name := zf.Name
		if zf.Flags&0x800 == 0 {
			name = decodeCP437(name)
		}
		name = strings.ReplaceAll(name, "\\", "/")
		isDir := strings.HasSuffix(name, "/")
		var parts []string
		bad := false
		for _, c := range strings.Split(name, "/") {
			switch c {
			case "", ".":
			case "..":
				bad = true
			default:
				parts = append(parts, c)
			}
		}
		if bad {
			warn("skipping %q: contains ..", zf.Name)
			continue
		}
		if len(parts) == 0 {
			continue
		}
		full := strings.Join(parts, "/")
		if isDir {
			if n, ok := nodes[full]; ok {
				if !n.IsDir {
					warn("skipping directory %q: a file has the same path", zf.Name)
				} else if n.ModTime.IsZero() {
					n.ModTime = zipTime(zf.Modified)
				}
				continue
			}
			d := getDir(full)
			if d == nil {
				warn("skipping directory %q: a file has the same path as a parent", zf.Name)
				continue
			}
			d.ModTime = zipTime(zf.Modified)
			continue
		}
		if zf.Flags&1 != 0 {
			warn("skipping %q: encrypted", zf.Name)
			continue
		}
		if zf.Method != zip.Store && zf.Method != zip.Deflate {
			warn("skipping %q: unsupported method %d", zf.Name, zf.Method)
			continue
		}
		if zf.UncompressedSize64 >= 1<<32 {
			warn("skipping %q: 4 GiB or larger", zf.Name)
			continue
		}
		if zf.Method == zip.Store && zf.CompressedSize64 != zf.UncompressedSize64 {
			warn("skipping %q: stored entry with inconsistent sizes", zf.Name)
			continue
		}
		if n, ok := nodes[full]; ok {
			if n.IsDir {
				warn("skipping file %q: a directory has the same path", zf.Name)
			} else {
				warn("skipping duplicate %q (keeping the first)", zf.Name)
			}
			continue
		}
		parent := getDir(dir0(full))
		if parent == nil {
			warn("skipping %q: a file has the same path as a parent", zf.Name)
			continue
		}
		s.files = append(s.files, zf)
		n := &Node{Name: parts[len(parts)-1], Size: int64(zf.UncompressedSize64),
			ModTime: zipTime(zf.Modified), ID: len(s.files) - 1}
		parent.Children = append(parent.Children, n)
		nodes[full] = n
	}
	s.offs = make([]atomic.Int64, len(s.files))
	// Extra and Comment are only read while the central directory is parsed;
	// dropping them frees the per-entry buffer they point into (about a third
	// of the retained zip directory).
	for _, zf := range s.files {
		zf.Extra, zf.Comment = nil, ""
	}
	return root, warns, nil
}

// inflate reads a whole deflated entry into cache blocks, verifying size and
// CRC.
func (s *zipSource) inflate(zf *zip.File) (*cdata, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Entries over the cache budget use the streaming path; never convert a
	// size that big to int (it can wrap negative on 32-bit systems).
	if zf.UncompressedSize64 > uint64(s.budget) {
		return nil, fmt.Errorf("%s: %s: entry of %d bytes exceeds the cache budget", s.zpath, zf.Name, zf.UncompressedSize64)
	}
	buf := s.cache.alloc(int(zf.UncompressedSize64))
	for off := 0; off < buf.n; off += blockSize {
		if _, err := io.ReadFull(rc, buf.blk[off/blockSize][:min(blockSize, buf.n-off)]); err != nil {
			s.cache.discard(buf)
			return nil, fmt.Errorf("%s: %s: %w", s.zpath, zf.Name, err)
		}
	}
	var one [1]byte
	if k, err := rc.Read(one[:]); k != 0 || err != io.EOF {
		if err == nil || err == io.EOF {
			err = errors.New("entry longer than its recorded size")
		}
		s.cache.discard(buf)
		return nil, fmt.Errorf("%s: %s: %w", s.zpath, zf.Name, err)
	}
	s.inflations.Add(1)
	return buf, nil
}

func (s *zipSource) ReadFileAt(n *Node, p []byte, off int64) (int, error) {
	if n.ID < 0 || n.ID >= len(s.files) {
		return 0, fmt.Errorf("tsshare: bad node id %d", n.ID)
	}
	if off < 0 {
		return 0, errors.New("tsshare: negative offset")
	}
	zf := s.files[n.ID]
	size := int64(zf.UncompressedSize64)
	if off >= size {
		return 0, io.EOF
	}
	short := false
	if int64(len(p)) > size-off {
		p = p[:size-off]
		short = true
	}
	var k int
	var err error
	switch {
	case zf.Method == zip.Store:
		k, err = s.readStored(n.ID, zf, p, off)
	case size > s.budget:
		k, err = s.readStream(n.ID, zf, p, off)
	default:
		err = s.cache.with(n.ID, func() (*cdata, error) { return s.inflate(zf) },
			func(d *cdata) { k = d.copyAt(p, off) })
	}
	if err == nil && short {
		err = io.EOF
	}
	return k, err
}

func (s *zipSource) readStored(id int, zf *zip.File, p []byte, off int64) (int, error) {
	base := s.offs[id].Load()
	if base == 0 {
		var err error
		if base, err = zf.DataOffset(); err != nil {
			return 0, err
		}
		s.offs[id].Store(base)
	}
	k, err := s.f.ReadAt(p, base+off)
	if k == len(p) {
		return k, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return k, err
}

// zipStream inflates a big entry (one larger than the cache budget) from the
// start through a sliding window. Each such entry keeps its 256 KB window and
// its open inflater for the life of the daemon (streams is never trimmed), so
// memory grows by that much per distinct huge entry read. No entry in the
// owner's SD.zip (largest 4.8 MB) is that big.
type zipStream struct {
	mu     sync.Mutex
	rc     io.ReadCloser
	win    []byte
	winOff int64 // offset of win[0]
	winLen int
	pos    int64 // stream position == winOff+winLen
	done   bool  // size reached and CRC verified
}

func (s *zipSource) readStream(id int, zf *zip.File, p []byte, off int64) (total int, err error) {
	s.smu.Lock()
	st := s.streams[id]
	if st == nil {
		st = &zipStream{}
		s.streams[id] = st
	}
	s.smu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			if st.rc != nil {
				st.rc.Close()
				st.rc = nil
			}
			st.winLen = 0
			err = fmt.Errorf("%s: %s: panic while inflating: %v", s.zpath, zf.Name, r)
		}
	}()
	size := int64(zf.UncompressedSize64)
	total = 0
	for total < len(p) {
		cur := off + int64(total)
		if st.winLen > 0 && cur >= st.winOff && cur < st.winOff+int64(st.winLen) {
			total += copy(p[total:], st.win[cur-st.winOff:st.winLen])
			continue
		}
		if st.rc == nil || cur < st.winOff {
			if st.rc != nil {
				st.rc.Close()
				st.rc = nil
			}
			rc, err := zf.Open()
			if err != nil {
				return total, err
			}
			st.rc, st.pos, st.winOff, st.winLen = rc, 0, 0, 0
		}
		if st.win == nil {
			st.win = make([]byte, windowSize)
		}
		want := int(min(int64(windowSize), size-st.pos))
		if want <= 0 {
			return total, io.ErrUnexpectedEOF
		}
		k, err := io.ReadFull(st.rc, st.win[:want])
		st.winOff, st.winLen = st.pos, k
		st.pos += int64(k)
		if err == nil && st.pos == size {
			var one [1]byte
			if c, e := st.rc.Read(one[:]); c != 0 || e != io.EOF {
				if e == nil || e == io.EOF {
					e = errors.New("entry longer than its recorded size")
				}
				err = e
			}
		}
		if err != nil {
			st.rc.Close()
			st.rc = nil
			st.winLen = 0
			return total, fmt.Errorf("%s: %s: %w", s.zpath, zf.Name, err)
		}
	}
	return total, nil
}

func (s *zipSource) Close() error {
	s.smu.Lock()
	for _, st := range s.streams {
		st.mu.Lock()
		if st.rc != nil {
			st.rc.Close()
		}
		st.mu.Unlock()
	}
	s.streams = map[int]*zipStream{}
	s.smu.Unlock()
	return s.f.Close()
}

// dir0 is path.Dir with "" for the top level.
func dir0(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
