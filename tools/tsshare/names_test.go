package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

// short renders an 11-byte alias as "STEM.EXT" for readable comparisons.
func short(b [11]byte) string {
	stem := strings.TrimRight(string(b[:8]), " ")
	ext := strings.TrimRight(string(b[8:]), " ")
	if ext == "" {
		return stem
	}
	return stem + "." + ext
}

func TestSanitizeName(t *testing.T) {
	long := strings.Repeat("a", 300)
	emoji := strings.Repeat("a", 254) + "\U0001F600" // 254 + 2 units = 256
	tests := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"clean", "hello.txt", "hello.txt", false},
		{"forbidden", `a\b/c:d*e?f"g<h>i|j`, "a_b_c_d_e_f_g_h_i_j", true},
		{"control", "a\x01b\x1fc", "a_b_c", true},
		{"trailing dot", "name.", "name", true},
		{"trailing dots and spaces", "name. . ", "name", true},
		{"leading space kept", " name", " name", false},
		{"300 chars", long, strings.Repeat("a", 255), true},
		{"surrogate at cut", emoji, strings.Repeat("a", 254), true},
		{"surrogate fits", strings.Repeat("a", 253) + "\U0001F600", strings.Repeat("a", 253) + "\U0001F600", false},
		{"empty", "", "_", true},
		{"only dots", "...", "_", true},
		{"cut then trailing dot", strings.Repeat("a", 254) + "." + "b", strings.Repeat("a", 254), true},
	}
	for _, tc := range tests {
		got, ch := SanitizeName(tc.in)
		if got != tc.want || ch != tc.changed {
			t.Errorf("%s: SanitizeName(%q) = %q,%v want %q,%v", tc.name, tc.in, got, ch, tc.want, tc.changed)
		}
	}
}

func TestAliasNT(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"README.TXT", "README.TXT"},
		{"readme.txt", "README.TXT"}, // single-case 8.3: no tail (LFN still emitted)
		{"Readme.txt", "README~1.TXT"},
		{"12345678.123", "12345678.123"},
		{"NOEXT", "NOEXT"},
		{"123456789.txt", "123456~1.TXT"},
		{"VERYLONGNAME.TXT", "VERYLO~1.TXT"},
		{"A very long file name.text", "AVERYL~1.TEX"},
		{"file name.txt", "FILENA~1.TXT"},
		{"file.tar.gz", "FILETA~1.GZ"},
		{"x.verylongext", "X~1.VER"},
		{"a+b.txt", "A_B~1.TXT"},
		{"[a].txt", "_A_~1.TXT"},
		{".gitignore", "GITIGN~1"},
		{"Привет.txt", "______~1.TXT"},
		{"\U0001F600a.txt", "_A~1.TXT"},
		{"game (1).z80", "GAME(1~1.Z80"},
	}
	for _, tc := range tests {
		got := short(NewNamer().Alias(tc.in))
		if got != tc.want {
			t.Errorf("Alias(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestAliasSequential(t *testing.T) {
	n := NewNamer()
	// Same basis repeatedly: ~1, ~2, ...
	for i, want := range []string{"LONGNA~1.TXT", "LONGNA~2.TXT", "LONGNA~3.TXT"} {
		got := short(n.Alias("longnamex" + strings.Repeat("x", i) + ".txt"))
		if got != want {
			t.Errorf("seq %d: got %q want %q", i, got, want)
		}
	}
	// An exact 8.3 name that collides with an earlier generated alias gets a tail.
	n = NewNamer()
	if g := short(n.Alias("Readme.txt")); g != "README~1.TXT" {
		t.Fatalf("got %q", g)
	}
	if g := short(n.Alias("README~1.TXT")); g != "README~2.TXT" {
		t.Errorf("collision with generated alias: got %q", g)
	}
	// Same exact name twice: second gets a tail.
	n = NewNamer()
	n.Alias("README.TXT")
	if g := short(n.Alias("README.TXT")); g != "README~1.TXT" {
		t.Errorf("duplicate: got %q", g)
	}
}

func TestAliasStorm(t *testing.T) {
	n := NewNamer()
	seen := map[string]bool{}
	for i := 1; i <= 200; i++ {
		name := fmt.Sprintf("XX S - Sword of Darkness (variant %d).vgm", i)
		a := n.Alias(name)
		s := short(a)
		var want string
		switch {
		case i < 10:
			want = fmt.Sprintf("XXS-SW~%d.VGM", i)
		case i < 100:
			want = fmt.Sprintf("XXS-S~%d.VGM", i)
		default:
			want = fmt.Sprintf("XXS-~%d.VGM", i)
		}
		if s != want {
			t.Fatalf("i=%d: got %q want %q", i, s, want)
		}
		if len(strings.TrimRight(string(a[:8]), " ")) > 8 || seen[string(a[:])] {
			t.Fatalf("i=%d: bad or duplicate alias %q", i, s)
		}
		seen[string(a[:])] = true
	}
	if len(seen) != 200 {
		t.Errorf("distinct = %d", len(seen))
	}
}

func TestAliasTailGrowth(t *testing.T) {
	// Names share a 6-char prefix; the 1000th gets 3 stem chars + ~1000.
	n := NewNamer()
	var last [11]byte
	for i := 1; i <= 1000; i++ {
		last = n.Alias(fmt.Sprintf("abcdefghijkl%d.dat", i))
	}
	if g := short(last); g != "ABC~1000.DAT" {
		t.Errorf("~1000: got %q", g)
	}
}

func TestUniqueCase(t *testing.T) {
	n := NewNamer()
	got := []string{n.Unique("A.TXT"), n.Unique("a.txt"), n.Unique("A.txt")}
	want := []string{"A.TXT", "a~1.txt", "A~2.txt"}
	seen := map[string]bool{}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("Unique #%d = %q want %q", i, g, want[i])
		}
		l := strings.ToLower(g)
		if seen[l] {
			t.Errorf("case-insensitive duplicate %q", g)
		}
		seen[l] = true
	}
	// No extension, and a name whose ~1 form is itself taken.
	n = NewNamer()
	n.Unique("foo~1")
	if a, b := n.Unique("FOO"), n.Unique("foo"); a != "FOO" || b != "foo~2" {
		t.Errorf("no-ext: %q %q", a, b)
	}
}

func TestShortChecksum(t *testing.T) {
	var spaces [11]byte
	for i := range spaces {
		spaces[i] = ' '
	}
	if g := ShortChecksum(spaces); g != 0xF7 { // worked by hand
		t.Errorf("spaces: %#x", g)
	}
	if g := ShortChecksum([11]byte{}); g != 0 {
		t.Errorf("zeros: %#x", g)
	}
}

func refChecksum(s [11]byte) byte {
	sum := 0
	for _, c := range s {
		sum = ((sum&1)*128 + sum/2 + int(c)) & 0xFF
	}
	return byte(sum)
}

// decodeLFN reassembles the name from entries (on-disk order) and checks the structure.
func decodeLFN(t *testing.T, es [][32]byte, sum byte) []uint16 {
	t.Helper()
	var units []uint16
	n := len(es)
	for i := 0; i < n; i++ { // i-th on disk holds sequence n-i
		e := es[i]
		seq := byte(n - i)
		wantSeq := seq
		if i == 0 {
			wantSeq |= 0x40
		}
		if e[0] != wantSeq {
			t.Errorf("entry %d: seq byte %#x want %#x", i, e[0], wantSeq)
		}
		if e[11] != 0x0F || e[12] != 0 || e[26] != 0 || e[27] != 0 {
			t.Errorf("entry %d: attr/type/cluster = %#x %#x %#x%#x", i, e[11], e[12], e[27], e[26])
		}
		if e[13] != sum {
			t.Errorf("entry %d: checksum %#x want %#x", i, e[13], sum)
		}
		var u []uint16
		for _, r := range [][2]int{{1, 5}, {14, 6}, {28, 2}} {
			for k := 0; k < r[1]; k++ {
				u = append(u, uint16(e[r[0]+2*k])|uint16(e[r[0]+2*k+1])<<8)
			}
		}
		units = append(u, units...) // lower sequence is earlier in the name
	}
	return units
}

func TestLFNEntries(t *testing.T) {
	nm := NewNamer()
	name20 := "abcdefghijklmnopqrst"
	name255 := strings.Repeat("0123456789abc", 19) + "defghijk" // 247 + 8
	exact26 := strings.Repeat("z", 26)
	emoji := strings.Repeat("a", 12) + "\U0001F600" // surrogate pair straddles entries 1/2
	tests := []struct {
		name    string
		long    string
		entries int
	}{
		{"20 chars", name20, 2},
		{"255 chars", name255, 20},
		{"exactly two full entries", exact26, 2},
		{"surrogate straddling", emoji, 2},
		{"non-BMP only", "\U0001F600.txt", 1},
	}
	for _, tc := range tests {
		if tc.name == "255 chars" && len([]rune(tc.long)) != 255 {
			t.Fatalf("bad test name length %d", len(tc.long))
		}
		sh := nm.Alias(tc.long)
		es := LFNEntries(tc.long, sh)
		if len(es) != tc.entries {
			t.Errorf("%s: %d entries want %d", tc.name, len(es), tc.entries)
			continue
		}
		if cs := ShortChecksum(sh); cs != refChecksum(sh) {
			t.Errorf("%s: ShortChecksum %#x want %#x", tc.name, cs, refChecksum(sh))
		}
		units := decodeLFN(t, es, refChecksum(sh))
		want := utf16.Encode([]rune(tc.long))
		if len(want)%13 != 0 {
			want = append(want, 0x0000)
			for len(want)%13 != 0 {
				want = append(want, 0xFFFF)
			}
		}
		if len(units) != len(want) {
			t.Fatalf("%s: unit count %d want %d", tc.name, len(units), len(want))
		}
		for i := range want {
			if units[i] != want[i] {
				t.Errorf("%s: unit %d = %#x want %#x", tc.name, i, units[i], want[i])
				break
			}
		}
	}
	// Explicit bytes for the 20-char name: first on disk is seq 0x42 carrying chars 14..20.
	es := LFNEntries(name20, nm.Alias(name20))
	if es[0][0] != 0x42 || es[1][0] != 0x01 {
		t.Errorf("seq bytes %#x %#x", es[0][0], es[1][0])
	}
	if es[0][1] != 'n' || es[0][2] != 0 || es[0][3] != 'o' { // 14th char is 'n'
		t.Errorf("first unit of last entry: % x", es[0][1:5])
	}
	if es[0][18] != 0x00 || es[0][19] != 0x00 || es[0][20] != 0xFF { // unit 7 is at offset 14+2*2
		t.Errorf("terminator/padding wrong: % x", es[0][:])
	}
}

func TestLFNEntriesNotNeeded(t *testing.T) {
	nm := NewNamer()
	for _, name := range []string{"README.TXT", "12345678.123", "NOEXT"} {
		if es := LFNEntries(name, nm.Alias(name)); len(es) != 0 {
			t.Errorf("%q: %d entries, want 0", name, len(es))
		}
	}
	for _, name := range []string{"readme.txt", "Readme.txt"} {
		if es := LFNEntries(name, NewNamer().Alias(name)); len(es) == 0 {
			t.Errorf("%q: want LFN entries", name)
		}
	}
}

func TestAliasFallbackNoPanic(t *testing.T) {
	// Push the ~N counter to where no stem room is left (~1000000 is 8 chars).
	n := NewNamer()
	n.next["LONGNA\x00TXT"] = 1000000
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := n.Alias("LongName.txt")
		s := short(a)
		if seen[s] {
			t.Fatalf("duplicate alias %q", s)
		}
		seen[s] = true
		if string(a[8:]) != "TXT" || !strings.Contains(string(a[:8]), "~") {
			t.Fatalf("bad fallback alias %q", s)
		}
		if i == 0 && !strings.HasPrefix(s, "LO") {
			t.Errorf("fallback alias %q should keep 2 stem chars", s)
		}
	}
	// Deterministic: the same history gives the same aliases.
	n2 := NewNamer()
	n2.next["LONGNA\x00TXT"] = 1000000
	n3 := NewNamer()
	n3.next["LONGNA\x00TXT"] = 1000000
	if n2.Alias("LongName.txt") != n3.Alias("LongName.txt") {
		t.Error("fallback is not deterministic")
	}
}
