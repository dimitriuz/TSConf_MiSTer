# tsshare performance results (host only)

Measured 2026-11-13 on the build host, **not on the MiSTer board**. Everything
about the ARM Cortex-A9 is an estimate or not measured; see the last sections.

Host: AMD Ryzen 9 6900HS (8 cores / 16 threads), 23 GB RAM, Linux 7.3.3, Go 1.27.3,
FUSE through `fusermount3` as a normal user (go-fuse, `FOPEN_DIRECT_IO`),
source NFSv4.0 mount (`rsize=262144`, LAN NAS). Scratch data on an NVMe disk.
Peak RSS is `VmHWM` from `/proc/<pid>/status` (there is no `/usr/bin/time`).

## Summary

| Item | Result |
| --- | --- |
| SD.zip start-up (scan + volume build, then `share.vhd` visible) | 0.42 s cold page cache, 0.35 s warm; "built in" 398 / 327 ms |
| SD.zip warnings | none (0) |
| SD.zip geometry | 57,353 files, 1,034 dirs (incl. root), 8 sectors/cluster, 912,542 clusters, 7,130 FAT sectors, partition at sector 128 (7,314,628 sectors), disk 3,745,513,472 bytes |
| Peak RSS, SD.zip, default 64 MB cache | 58 MB after the scan, 135 MB after one full sequential read, 145.5 MB at exit after a whole verification session (see "Memory") |
| `fsck.vfat -n` on SD.zip partition | clean, exit 0: `58387 files, 912542/912542 clusters` |
| Content check against the zip | all 57,353 entries equal (md5, Python `zipfile` as the independent reader); includes 300 random, the 20 largest, all 29 non-ASCII names |
| Warm 16 KB read | server-side median 1-2 us, p99 10-13 us; client-side (through FUSE) median 14-18 us, p99 51-117 us, max 0.84 ms |
| Cold whole 3 MB read from the NAS-backed zip | 17-39 ms (worst, a 4.0 MB entry with 2.5 MB deflated: 97 ms) against a bar of 10 s |
| Latency bars | met by a wide margin; no latency fix was needed |
| ARM binary | 3.0 MB (`-trimpath -ldflags='-s -w'`), 4.6 MB unstripped |

## 1. Scale tree (12 GB, sparse, directory source)

Generator: `go run ./scale <dir>` (`scale/gentree.go`, deterministic): 5,000 files
in 300 directories (depth up to 6), 12.00 GiB apparent size, as sparse files with
four 64-byte path-derived stamps each (start, 1/3, middle, end) so a shifted or
misplaced extent changes the md5 although the files stay sparse. Names: very long
(100-145 chars), long shared stems, case twins (`Twin5.Txt` / `TWIN5.TXT`), many
spaces, non-ASCII (Cyrillic, Japanese, accents), an `XX S - Sword of Darkness N.vgm`
storm, 52 empty files, files of exact 4096 and 65536 multiples, files of 100-800
MB; mtimes spread over 1975-2030.

```
tsshare -src tree -mount mnt1 -log scale.log
```

| Measure | Result |
| --- | --- |
| Start-up (mount visible) | 0.13 s wall; "built in 111 ms" (49-59 ms in earlier runs) |
| Geometry | 5,000 files, 300 dirs (incl. root), 8 sectors/cluster, 3,148,425 clusters, 24,598 FAT sectors, disk 12,921,602,048 bytes |
| Warnings | 9, all "renamed" (the generator cuts some long names after a space; the trailing space is trimmed) |
| Peak RSS | 17 MB after start-up; 38 MB after reading the whole 12.9 GB (`dd bs=1M`, 8.8 s, 1.5 GB/s); the first binary measured 20 MB / 58-63 MB |
| `fsck.vfat -n` (partition extracted with `dd bs=1M iflag=skip_bytes,count_bytes skip=65536`) | clean, exit 0: `5300 files, 3148425/3148425 clusters` (0.2 s) |
| 200 random files `mcopy` out of `share.vhd@@65536` vs source md5 | 200 ok, 0 bad (twin names excluded since mtools matches case-insensitively) |
| independent FAT reader (`scale/fatcheck.py tree`), 200 random | 199 ok, 0 mismatch, 1 renamed (not found by exact path) |
| independent FAT reader, **all** 5,000 files (md5, size and FAT mtime vs source) | 4,991 ok, 0 mismatch, 9 renamed (the 9 warnings), 0 mtime differences |

## 2. The real zip (SD.zip, 1.4 GB, NFS, read only)

`tsshare -src .../AAA/SD.zip -mount mnt2`, final binary.

Start-up: the zip's page cache was evicted before the cold run with
`dd if=SD.zip iflag=nocache count=0` (verified with `fincore`; the NAS server's own
cache is not controlled). Cold: 0.417 s to `share.vhd`, "built in 398 ms"
(5.7 MB of central directory read over NFS). Warm: 0.354 s, "built in 327 ms"
(0.31 s in another run).

Printed counts: `tree: 57353 files, 1034 directories (including the root)`;
`volume: 8 sectors/cluster, 912542 clusters, 7130 FAT sectors, partition at sector 128, disk 3745513472 bytes`.
**Warnings: 0.** No name was renamed, truncated or dropped (longest name 149 chars,
longest path 200 chars, 29 non-ASCII entries); the zip has no duplicate,
encrypted, `..`, oversized or unsupported entries.

Whole-disk sequential reads (the content is inflated on demand, so this inflates
the whole 3.36 GiB):

| Run | Wall | Throughput | Daemon CPU (user + sys) | Peak RSS |
| --- | --- | --- | --- | --- |
| `dd if=share.vhd of=/dev/null bs=128k`, final binary | 22.0 s | 170 MB/s | 18.98 + 1.54 s | 135 MB |
| `dd` of the partition to disk, `bs=1M` | 25.8 s | 145 MB/s | | 135.9 MB |
| the same `dd bs=128k` before the memory fix | 29.6 s | 126 MB/s | 23.45 + 2.32 s | 266 MB |

RSS at the milestones (final binary): 52-58 MB right after the scan (the
scan's garbage is returned to the OS; HWM 57.9 MB); 135 MB after one full read;
**145.5 MB** (HWM at exit) after the whole verification session, which in
addition did a second full read, `fsck`, a Python pass over all 57,353 entries and
`mdir -/`. With `-cache-mb 32`: 97.6 MB after a full read.

`fsck.vfat -n` on the extracted 3.4 GiB partition (0.8 s), exact output, exit 0:

```
fsck.fat 4.2 (2021-01-31)
zpart.img: 58387 files, 912542/912542 clusters
```

Content: `scale/fatcheck.py zip` reads the FAT chain of every compared file straight
from the mounted `share.vhd` and compares md5 against Python's `zipfile` (so names
with non-ASCII characters and long names are matched by exact path inside the
zip, and every LFN checksum is verified while walking):

```
zip random: 300 checked, ok 300, mismatch 0, not-found 0
zip top: 20 checked, ok 20, mismatch 0, not-found 0
zip non-ASCII: 29 checked, ok 29, mismatch 0, not-found 0
zip all: 57353 checked, ok 57353, mismatch 0, not-found 0
```

`mdir -i share.vhd@@65536 -/ -b ::` lists 58,386 names (57,353 files + 1,033
directories), instantly.

## 3. Latency

`tsshare -v -log lat.log`, SD.zip with its page cache evicted first. Tool:
`scale/fatcheck.py seq|rand`, which maps each file through the FAT, reads it
through the mount with `pread` in 16 KB chunks and joins the client timing with the
daemon's read log by offset (server latency = time inside `vhdFile.Read`; client
latency adds the kernel and FUSE round trip; "cold" = the first read of an
entry's file, which inflates the whole entry, "warm" = any later read).

Sequential: the 20 largest files (2-4.8 MB) in random order (seed 11), each read
once in 16 KB chunks (cold) and straight again (warm); 3,640 reads per pass.

| | median | p95 | p99 | max (us) |
| --- | --- | --- | --- | --- |
| cold, client | 15 | 36 | 85 | 91,947 |
| cold, server (log) | 1 | 4 | 14 | 91,886 |
| warm, client | 14 | 28 | 51 | 838 |
| warm, server (log) | 1 | 5 | 10 | 47 |

Each cold file's single slow request is its first read; the rest of its chunks
run at warm speed. Whole-file wall time, cold: Ygor (3.44 MB) 24 ms,
demorama_TS.spg (3.24 MB) 39 ms, mindwarp.spg (2.23 MB) 26 ms, XX S - Sword of
Darkness (4.79 MB) 28-30 ms, ts_frac3.spg (4.00 MB, 2.5 MB deflated) 97 ms, the
rest 12-30 ms; warm: 2-6 ms. The largest compressed entry is 2.5 MB, and all
stored entries are under 8 KB, so the NFS-heavy cases are covered. **A cold 3 MB read
takes about 20-40 ms (bar: under about 10 s).**

Random: 3,000 reads of 16 KB at random 512-byte-aligned offsets inside the extents
of 300 random files (>= 16 KB), random file order (seed 5), 300 first touches:

| | median | p95 | p99 | max (us) |
| --- | --- | --- | --- | --- |
| cold (first touch of a file), client | 2,316 | 12,173 | 18,245 | 24,625 |
| cold, server | 2,244 | 12,084 | 18,123 | 24,554 |
| warm, client | 18 | 51 | 117 | 711 |
| warm, server | 2 | 7 | 13 | 91 |

Warm reads are 2-3 orders of magnitude under the 1 ms bar; no latency fix was
made. (The sequential cold maximum, 92 ms, includes the first-touch inflate of a
4 MB entry.)

## 4. Fixes made (memory only)

**Problem.** After one sequential read of SD.zip the daemon had a peak RSS of
262-266 MB (85-89 MB after the scan), above the 150 MB budget. The heap profile
showed `zipSource.inflate` allocating a fresh `[]byte` per entry (3.4 GB of
garbage over a full read, 89% of all allocation); with live data of about 110 MB
(64 MB cache + 45 MB tree) the collector let the heap grow to about twice that.
GC tuning alone (GOGC 25: 192 MB; GOGC 50: 232 MB) did not fix it.

**Changes** (`cache.go`, `zipsource.go`, `main.go`; tests in `sources_test.go`):

1. Inflated entries are stored in 16 KiB blocks; the blocks of evicted entries go to
   a free list (at most `min(cache/16, 4 MiB)`) and are reused by the next
   inflate. Readers `pin` an entry while they copy from it (`byteCache.with`), so
   a buffer is never recycled under a reader; the cache bound counts block
   capacity, i.e. real memory. Allocation fell from 3.4 GB to about 70 MB per full read.
2. The zip's per-entry `Extra` and `Comment` are dropped after the scan (they are
   only read while parsing the central directory and kept a buffer alive per
   entry).
3. `debug.FreeOSMemory()` after the volume is built (the scan's garbage), and
   `debug.SetGCPercent(10)` unless `GOGC` is set, because the live heap is nearly
   all cache and tree. GC cost: the daemon CPU for a full read stayed at about
   20 s.
4. A `pprof` build tag (`go build -tags pprof`, env `TSSHARE_PPROF=127.0.0.1:6060`)
   for heap/CPU profiles without linking `net/http` into the release binary; and
   benchmarks (`perf_test.go`).

Result: 135 MB after a full read (was 266 MB), 58 MB after the scan (was 89 MB),
throughput 170 MB/s (was 126 MB/s: less GC work).

## 5. Board build and estimated ARM figures

```
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o tsshare-arm .
```

3,014,816 bytes (statically linked ELF 32-bit ARM EABI5); without `-s -w`
4,633,611 bytes. The host binary is 4.9 MB (3.1 MB stripped).

Host scan + volume build for SD.zip (`TSSHARE_BENCH_ZIP=... go test -run xxx -bench
RealZip -benchtime 10x`, zip in the page cache, so CPU only): 201-207 ms,
139 MB allocated in total, 1.73 M allocations. The synthetic 57,000-file tree
(`BenchmarkNewVolumeSynthetic`): 156-158 ms.

**Estimate, not a measurement:** a Cortex-A9 at about 1 GHz is roughly 10-15 times
slower than this core for Go code of this kind (pointer chasing, string building,
allocation), so the scan + build should take about 2-4 s on the board plus the
network read of the 5.7 MB central directory. The inflate rate (host: about 175
MB/s of output per core) would be about 12-18 MB/s there, so a cold 3 MB entry
costs on the order of 0.3-1.5 s plus network, and a full read of the whole zip
would take minutes. These must be measured on the board
(`GOOS=linux GOARCH=arm GOARM=7 go test -c -o tsshare.test`, run
`./tsshare.test -test.run xxx -test.bench Synthetic`, and the start-up line of the
real daemon).

## 6. What was NOT measured

- Anything on the board: ARM speed, the board's NFS/CIFS client, the real FUSE
  kernel module there (no `fusermount`, direct mount), 491 MB RAM pressure with the
  rest of MiSTer running, the SD core's actual read pattern and timing.
- A truly cold NAS: the zip's page cache was evicted, but the NAS server's own
  cache and the network path were not.
- A slow or WAN source, or a directory source over NFS.
- Concurrent readers on the host (the latency runs are single-threaded).
- Time zones other than the host's (mtime checks used the host's local time).

## 7. Reproducing

```
cd tools/tsshare
go run ./scale ~/scratch/tree                      # 12 GiB sparse tree
go build -o tsshare . && ./tsshare -src ~/scratch/tree -mount ~/scratch/mnt -log run.log   # add -v for read lines
python3 -I scale/fatcheck.py tree ~/scratch/mnt/share.vhd ~/scratch/tree 5000 3
python3 -I scale/fatcheck.py zip  ~/scratch/mnt/share.vhd SD.zip 300 7 20 all
python3 -I scale/fatcheck.py seq  ~/scratch/mnt/share.vhd run.log 20 11
python3 -I scale/fatcheck.py rand ~/scratch/mnt/share.vhd run.log 3000 300 5
dd if=mnt/share.vhd of=part.img bs=1M iflag=skip_bytes,count_bytes skip=65536 count=$((SECTORS*512)); fsck.vfat -n part.img
```

`SECTORS` is the partition size in the MBR (bytes 458-461 of `share.vhd`).

## 8. Warnings produced by the real zip

None. The run printed `0 warnings`.
