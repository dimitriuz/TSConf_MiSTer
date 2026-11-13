package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// dirSource serves a directory tree. ModTime fields are local wall-clock
// (info.ModTime().In(time.Local)), which Volume writes to FAT unchanged.
type dirSource struct {
	root  string
	paths []string // by Node.ID
	cache *fdCache
}

// NewDirSource scans root recursively. Symlinks to files are followed (a
// warning only when broken); symlinked directories and entries whose Info
// fails are skipped with warnings. Files are not opened at scan time, so a
// file that cannot be opened shows up as an error when it is read.
//
// Reads are clamped to the size seen at scan time: a file that grew never
// yields bytes beyond Size, and a file that shrank makes the read fail.
func NewDirSource(root string, o SourceOptions) (Source, error) {
	o = o.withDefaults()
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("tsshare: %s is not a directory", root)
	}
	return &dirSource{root: root, cache: newFDCache(o.FDLimit)}, nil
}

func (s *dirSource) Tree() (*Node, []string, error) {
	s.paths = nil
	var warns []string
	warn := func(f string, a ...any) {
		warns = append(warns, s.root+": "+fmt.Sprintf(f, a...))
	}
	st, err := os.Stat(s.root)
	if err != nil {
		return nil, nil, err
	}
	root := &Node{IsDir: true, ModTime: st.ModTime().In(time.Local)}
	var walk func(dir string, n *Node)
	walk = func(dir string, n *Node) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			warn("skipping unreadable directory %s: %v", dir, err)
			return
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				warn("skipping %s: %v", p, err)
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				ti, err := os.Stat(p)
				if err != nil {
					warn("skipping broken symlink %s", p)
					continue
				}
				if ti.IsDir() {
					warn("skipping symlinked directory %s", p)
					continue
				}
				info = ti
			}
			switch {
			case info.IsDir():
				c := &Node{Name: e.Name(), IsDir: true, ModTime: info.ModTime().In(time.Local)}
				n.Children = append(n.Children, c)
				walk(p, c)
			case info.Mode().IsRegular():
				if info.Size() >= 1<<32 {
					warn("skipping %s: 4 GiB or larger", p)
					continue
				}
				s.paths = append(s.paths, p)
				n.Children = append(n.Children, &Node{
					Name: e.Name(), Size: info.Size(),
					ModTime: info.ModTime().In(time.Local), ID: len(s.paths) - 1,
				})
			default:
				warn("skipping %s: not a regular file", p)
			}
		}
	}
	walk(s.root, root)
	return root, warns, nil
}

func (s *dirSource) ReadFileAt(n *Node, p []byte, off int64) (int, error) {
	if n.ID < 0 || n.ID >= len(s.paths) {
		return 0, fmt.Errorf("tsshare: bad node id %d", n.ID)
	}
	if off < 0 {
		return 0, fmt.Errorf("tsshare: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	short := false
	if off >= n.Size {
		return 0, io.EOF
	}
	if int64(len(p)) > n.Size-off {
		p = p[:n.Size-off]
		short = true
	}
	k, err := s.readClamped(n, p, off)
	if err == nil && short {
		err = io.EOF
	}
	return k, err
}

func (s *dirSource) readClamped(n *Node, p []byte, off int64) (int, error) {
	e := s.cache.get(n.ID)
	defer s.cache.put(e)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		f, err := os.Open(s.paths[n.ID])
		if err != nil {
			return 0, err
		}
		e.f = f
		e.winLen = 0
	}
	total := 0
	for total < len(p) {
		cur := off + int64(total)
		if cur >= n.Size {
			return total, io.EOF
		}
		if e.winLen > 0 && cur >= e.winOff && cur < e.winOff+int64(e.winLen) {
			total += copy(p[total:], e.win[cur-e.winOff:e.winLen])
			continue
		}
		if len(p)-total >= windowSize {
			k, err := e.f.ReadAt(p[total:], cur)
			total += k
			if total == len(p) {
				return total, nil
			}
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return total, err
		}
		if e.win == nil {
			e.win = make([]byte, windowSize)
		}
		want := int(min(int64(windowSize), n.Size-cur))
		k, err := e.f.ReadAt(e.win[:want], cur)
		e.winOff, e.winLen = cur, k
		if k == 0 {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return total, err
		}
	}
	return total, nil
}

func (s *dirSource) Close() error {
	s.cache.closeAll()
	return nil
}
