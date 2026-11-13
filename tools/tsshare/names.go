// Package tsshare builds a read-only FAT32 view of a folder or zip.
package main

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf16"
)

// SanitizeName makes name legal as a FAT long file name: FAT-forbidden
// characters and control characters become '_', the result is cut to 255
// UTF-16 units (never inside a surrogate pair), trailing dots and spaces are
// trimmed, and an empty result becomes "_". Leading spaces are kept.
func SanitizeName(name string) (clean string, changed bool) {
	var b []rune
	units := 0
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			r = '_'
		}
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > 255 {
			break
		}
		units += w
		b = append(b, r)
	}
	for len(b) > 0 && (b[len(b)-1] == '.' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		b = []rune{'_'}
	}
	clean = string(b)
	return clean, clean != name
}

// Namer hands out 8.3 aliases and case-insensitively unique long names for
// one directory.
type Namer struct {
	used  map[[11]byte]bool // aliases handed out
	next  map[string]int    // next ~N to try, per 6-char stem prefix + ext
	names map[string]bool   // lower-cased long names seen by Unique
}

func NewNamer() *Namer {
	return &Namer{
		used:  map[[11]byte]bool{},
		next:  map[string]int{},
		names: map[string]bool{},
	}
}

// valid8dot3 reports whether c may appear in a short name as-is (upper-case
// letters, digits and the punctuation FAT allows). Lower-case is handled by
// the caller.
func valid8dot3(c rune) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'()-@^_`{}~", c)
}

// basis computes the NT-style stem and extension (upper-case, ASCII only,
// untruncated except that nothing is cut here) and whether the conversion was
// lossy, i.e. whether the alias needs a ~N tail.
//
// Choices the plan left open:
//   - a name that is valid 8.3 in a single case (all letters upper, or all
//     lower) is not lossy; mixed-case names ("Readme.txt") are, giving
//     README~1.TXT, as Windows does for names without case flags.
//   - each non-ASCII rune becomes one '_' (a surrogate pair is one rune).
//   - leading dots are dropped, so ".gitignore" becomes stem GITIGNORE.
//   - invalid ASCII such as + , ; = [ ] becomes '_'.
func basis(long string) (stem, ext string, lossy bool) {
	rs := []rune(long)
	i := 0
	for i < len(rs) && rs[i] == '.' {
		i++
		lossy = true
	}
	rs = rs[i:]
	dot := -1
	for k, r := range rs {
		if r == '.' {
			dot = k
		}
	}
	var hasLower, hasUpper bool
	conv := func(part []rune, isStem bool) string {
		var sb strings.Builder
		for _, r := range part {
			switch {
			case r == ' ' || r == '.':
				lossy = true
				continue
			case r >= 'a' && r <= 'z':
				hasLower = true
				r -= 'a' - 'A'
			case r >= 'A' && r <= 'Z':
				hasUpper = true
			}
			if !valid8dot3(r) {
				r = '_'
				lossy = true
			}
			sb.WriteRune(r)
		}
		return sb.String()
	}
	if dot < 0 {
		stem = conv(rs, true)
	} else {
		stem = conv(rs[:dot], true)
		ext = conv(rs[dot+1:], false)
	}
	if stem == "" {
		stem = "_"
		lossy = true
	}
	if hasLower && hasUpper {
		lossy = true
	}
	if len(stem) > 8 || len(ext) > 3 {
		lossy = true
	}
	return stem, ext, lossy
}

func pack(stem, ext string) [11]byte {
	var b [11]byte
	for i := range b {
		b[i] = ' '
	}
	copy(b[:8], stem)
	copy(b[8:], ext)
	return b
}

// Alias returns an 8.3 alias unique within this Namer. long must already be
// sanitized (see SanitizeName). The stem keeps up to 6 characters plus ~N,
// shrinking as N grows so the stem is at most 8 bytes (5+~10, 4+~100,
// 3+~1000, ...). Once N reaches 1000000 no stem character would fit, and
// Alias switches to a deterministic hash alias: the first 2 stem characters,
// 4 hex digits of a hash of long, and ~1 (then ~2.. on a clash).
func (n *Namer) Alias(long string) (short [11]byte) {
	stem, ext, lossy := basis(long)
	if !lossy {
		c := pack(stem, ext)
		if !n.used[c] {
			n.used[c] = true
			return c
		}
	}
	if len(ext) > 3 {
		ext = ext[:3]
	}
	p := stem
	if len(p) > 6 {
		p = p[:6]
	}
	key := p + "\x00" + ext
	for N := max(n.next[key], 1); ; N++ {
		tail := "~" + strconv.Itoa(N)
		keep := min(8-len(tail), 6, len(stem))
		if keep < 1 {
			return n.hashAlias(long, stem, ext)
		}
		c := pack(stem[:keep]+tail, ext)
		if !n.used[c] {
			n.used[c] = true
			n.next[key] = N + 1
			return c
		}
	}
}

// hashAlias is Alias's last resort when the ~N counter has no room left.
func (n *Namer) hashAlias(long, stem, ext string) [11]byte {
	if len(stem) > 2 {
		stem = stem[:2]
	}
	for salt := 0; ; salt++ {
		h := fnv.New32a()
		h.Write([]byte(long))
		h.Write([]byte{byte(salt), byte(salt >> 8), byte(salt >> 16)})
		sum := h.Sum32()
		tail := 1 + salt/65536%9
		c := pack(fmt.Sprintf("%s%04X~%d", stem, sum&0xFFFF, tail), ext)
		if !n.used[c] {
			n.used[c] = true
			return c
		}
	}
}

// Unique returns long, or long with ~N before the extension if a name equal
// to it ignoring case was already seen, so no two entries differ only by case.
func (n *Namer) Unique(long string) string {
	if !n.names[strings.ToLower(long)] {
		n.names[strings.ToLower(long)] = true
		return long
	}
	base, ext := long, ""
	if d := strings.LastIndexByte(long, '.'); d > 0 {
		base, ext = long[:d], long[d:]
	}
	for N := 1; ; N++ {
		c := base + "~" + strconv.Itoa(N) + ext
		if !n.names[strings.ToLower(c)] {
			n.names[strings.ToLower(c)] = true
			return c
		}
	}
}

// ShortChecksum is the LFN checksum of an 11-byte short name.
func ShortChecksum(short [11]byte) byte {
	var sum byte
	for _, c := range short {
		sum = (sum&1)<<7 | sum>>1
		sum += c
	}
	return sum
}

// shortString renders an alias as NAME.EXT.
func shortString(s [11]byte) string {
	stem := strings.TrimRight(string(s[:8]), " ")
	ext := strings.TrimRight(string(s[8:]), " ")
	if ext == "" {
		return stem
	}
	return stem + "." + ext
}

// LFNEntries returns the long-name directory entries in on-disk order (last
// sequence first, flagged 0x40). It returns none when long equals the
// rendering of short exactly (an upper-case 8.3 name); lower-case 8.3 names
// still get LFN entries. long must already be sanitized (at most 255 UTF-16
// units).
func LFNEntries(long string, short [11]byte) [][32]byte {
	if long == shortString(short) {
		return nil
	}
	u := utf16.Encode([]rune(long))
	cnt := (len(u) + 12) / 13
	padded := make([]uint16, cnt*13)
	for i := range padded {
		switch {
		case i < len(u):
			padded[i] = u[i]
		case i == len(u):
			padded[i] = 0
		default:
			padded[i] = 0xFFFF
		}
	}
	sum := ShortChecksum(short)
	out := make([][32]byte, 0, cnt)
	for seq := cnt; seq >= 1; seq-- {
		var e [32]byte
		e[0] = byte(seq)
		if seq == cnt {
			e[0] |= 0x40
		}
		e[11] = 0x0F
		e[13] = sum
		chunk := padded[(seq-1)*13 : seq*13]
		put := func(off int, us []uint16) {
			for k, v := range us {
				e[off+2*k] = byte(v)
				e[off+2*k+1] = byte(v >> 8)
			}
		}
		put(1, chunk[0:5])
		put(14, chunk[5:11])
		put(28, chunk[11:13])
		out = append(out, e)
	}
	return out
}
