// gentree writes a scale-test tree of sparse files: go run ./scale <dir>
// [files] [dirs] [total GB]. The tree is deterministic (fixed seed). File
// contents are zeros (sparse), so md5 comparison is cheap but still checks
// sizes, names and that every byte of the extent reads back.
package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gentree <dir> [files=5000] [dirs=300] [totalGB=12]")
		os.Exit(2)
	}
	root := os.Args[1]
	arg := func(i, def int) int {
		if len(os.Args) > i {
			if n, err := strconv.Atoi(os.Args[i]); err == nil {
				return n
			}
		}
		return def
	}
	nfiles, ndirs, gb := arg(2, 5000), arg(3, 300), arg(4, 12)
	rng := rand.New(rand.NewSource(20261113))

	// directory tree: each new dir hangs off a random earlier one (depth <= 6)
	dirs := []string{root}
	depth := map[string]int{root: 0}
	for len(dirs) < ndirs {
		p := dirs[rng.Intn(len(dirs))]
		if depth[p] >= 6 {
			continue
		}
		n := fmt.Sprintf("%s %02d", pick(rng, dirStems), len(dirs))
		if rng.Intn(10) == 0 {
			n = strings.Repeat("Long directory name ", 10)[:100+rng.Intn(40)] + fmt.Sprint(len(dirs))
		}
		d := filepath.Join(p, n)
		dirs = append(dirs, d)
		depth[d] = depth[p] + 1
	}
	for _, d := range dirs {
		must(os.MkdirAll(d, 0o755))
	}

	// sizes: log-uniform 0..~10 MB, rescaled to the byte target, plus a few
	// huge ones and cluster-exact ones
	target := int64(gb) << 30
	sizes := make([]int64, nfiles)
	var sum int64
	for i := range sizes {
		switch {
		case i%97 == 0:
			sizes[i] = 0
		case i%53 == 0:
			sizes[i] = int64(1+rng.Intn(2000)) * 4096 // exact cluster multiple
		case i%211 == 0:
			sizes[i] = int64(1+rng.Intn(20)) * 65536
		case i%401 == 0:
			sizes[i] = 100<<20 + rng.Int63n(700<<20) // very large
		default:
			sizes[i] = int64(float64(1000) * pow(rng.Float64()*9.5))
		}
		sum += sizes[i]
	}
	// scale the ordinary files so the total hits the target
	var fixed, var_ int64
	for i, s := range sizes {
		if i%97 == 0 || i%53 == 0 || i%211 == 0 || i%401 == 0 {
			fixed += s
		} else {
			var_ += s
		}
	}
	if var_ > 0 && target > fixed {
		k := float64(target-fixed) / float64(var_)
		for i := range sizes {
			if !(i%97 == 0 || i%53 == 0 || i%211 == 0 || i%401 == 0) {
				sizes[i] = int64(float64(sizes[i]) * k)
			}
		}
	}

	start := time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC)
	span := time.Date(2030, 12, 31, 0, 0, 0, 0, time.UTC).Sub(start)
	used := map[string]bool{}
	var total int64
	made := 0
	for i := 0; made < nfiles; i++ {
		d := dirs[rng.Intn(len(dirs))]
		var name string
		switch rng.Intn(12) {
		case 0: // long stem shared by many names
			name = "Some very long shared stem for a compilation title - disc " + fmt.Sprint(rng.Intn(60)) + " (variant " + fmt.Sprint(i) + ")" + pick(rng, exts)
		case 1: // very long, up to 149 chars
			name = (strings.Repeat("Long name segment ", 9) + fmt.Sprint(i))[:100+rng.Intn(45)] + pick(rng, exts)
		case 2: // spaces
			name = "A  file  with   spaces " + fmt.Sprint(i) + pick(rng, exts)
		case 3: // non-ASCII
			name = pick(rng, uni) + " " + fmt.Sprint(i) + pick(rng, exts)
		case 4: // case twins
			base := "Twin" + fmt.Sprint(i/2) + ".Txt"
			if i%2 == 0 {
				name = base
			} else {
				name = strings.ToUpper(base)
			}
		case 5: // storm: same stem, short
			name = "XX S - Sword of Darkness " + fmt.Sprint(i) + ".vgm"
		default:
			name = fmt.Sprintf("%s %d%s", pick(rng, fileStems), i, pick(rng, exts))
		}
		p := filepath.Join(d, name)
		if used[p] {
			continue
		}
		size := sizes[made]
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		must(f.Truncate(size))
		stamp(f, p, size)
		must(f.Close())
		mt := start.Add(time.Duration(rng.Int63n(int64(span))))
		must(os.Chtimes(p, mt, mt))
		used[p] = true
		total += size
		made++
	}
	fmt.Printf("%d files, %d dirs, %.2f GiB\n", made, len(dirs), float64(total)/(1<<30))
}

func pow(x float64) float64 { return math.Pow(10, x*4/9.5) }

// stamp writes small path-derived blocks at the start, middle and end of the
// file, so a misplaced or shifted extent changes the md5 although the file
// stays sparse.
func stamp(f *os.File, p string, size int64) {
	if size == 0 {
		return
	}
	for _, off := range []int64{0, size / 2, size - 64, size/3 + 1} {
		if off < 0 || off+64 > size {
			continue
		}
		var b [64]byte
		h := fnv.New64a()
		fmt.Fprintf(h, "%s@%d", p, off)
		x := h.Sum64()
		for i := range b {
			x = x*6364136223846793005 + 1442695040888963407
			b[i] = byte(x >> 33)
		}
		_, err := f.WriteAt(b[:], off)
		must(err)
	}
}

func pick(r *rand.Rand, s []string) string { return s[r.Intn(len(s))] }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var (
	dirStems  = []string{"Games", "Demos", "Music", "TRD", "Tape images", "Utilities", "Collection", "Magazines", "Русские игры", "Docs"}
	fileStems = []string{"Game", "Demo", "Track", "Disk image", "Readme", "Loader", "Level pack", "Soundtrack"}
	exts      = []string{".trd", ".scl", ".tap", ".vgm", ".pt3", ".spg", ".txt", ".zip", ".a", ""}
	uni       = []string{"Музыка", "Café déjà vu", "日本語", "Ünïcödé", "Песня №1"}
)
