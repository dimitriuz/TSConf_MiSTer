package main

import (
	"fmt"
	"os"
	"testing"
)

// BenchmarkNewVolumeSynthetic builds the volume of a 57,000-file tree shaped
// like the owner's SD.zip (33 x 31 directories, long names); run it on the
// board with a cross-compiled test binary (go test -c) to measure NewVolume
// there.
func BenchmarkNewVolumeSynthetic(b *testing.B) {
	root := &Node{IsDir: true}
	id := 0
	for d1 := 0; d1 < 33; d1++ {
		a := &Node{Name: fmt.Sprintf("Collection %02d", d1), IsDir: true}
		root.Children = append(root.Children, a)
		for d2 := 0; d2 < 31; d2++ {
			c := &Node{Name: fmt.Sprintf("Sub collection %02d", d2), IsDir: true}
			a.Children = append(a.Children, c)
			for f := 0; f < 55; f++ {
				c.Children = append(c.Children, &Node{
					Name: fmt.Sprintf("Some game title %02d.%02d (1991)(Publisher)[a%d].trd", d2, f, f),
					Size: int64(10000 + f*1000), ModTime: t0, ID: id})
				id++
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := NewVolume(fixedSource{newMemSource(nil), root}, VolumeOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRealZip measures zip directory scan + NewVolume on the archive
// named by TSSHARE_BENCH_ZIP (skipped when unset). The file stays in the page
// cache after the first iteration, so this is the CPU cost, not the network.
func BenchmarkRealZip(b *testing.B) {
	p := os.Getenv("TSSHARE_BENCH_ZIP")
	if p == "" {
		b.Skip("TSSHARE_BENCH_ZIP not set")
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s, err := OpenSource(p, SourceOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := NewVolume(s, VolumeOptions{}); err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}
