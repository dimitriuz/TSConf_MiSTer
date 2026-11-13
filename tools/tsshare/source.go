package tsshare

import "time"

// Node is one file or directory of a source tree.
type Node struct {
	Name     string
	IsDir    bool
	Size     int64 // files
	ModTime  time.Time
	Children []*Node // dirs
	ID       int     // set by the Source; opaque to Volume
}

// Source provides the tree and the file contents for a Volume.
type Source interface {
	// Tree returns the root directory (its Name is ignored) and warnings.
	Tree() (root *Node, warnings []string, err error)
	// ReadFileAt reads file n at off; it must return len(p) bytes unless
	// it fails (io.EOF together with a full read is fine). It must be safe
	// for concurrent use.
	ReadFileAt(n *Node, p []byte, off int64) (int, error)
	Close() error
}
