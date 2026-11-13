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
| Peak RSS, SD.zip, default **32 MB** cache (`-mem-limit-mb 192`) | 58 MB after the scan, 100 MB after one full sequential read, 105 MB at exit after a verification session. With the former 64 MB default: 58 / 135 / 145.5 MB (see section 2 and 4) |
| `fsck.vfat -n` on SD.zip partition | clean, exit 0: `58387 files, 912542/912542 clusters` |
| Content check against the zip | all 57,353 entries equal (md5, Python `zipfile` as the independent reader); includes 300 random, the 20 largest, all 29 non-ASCII names |
| Warm 16 KB read | server-side median 0-2 us, p99 3-13 us; client-side (through FUSE) median 13-18 us, p99 35-117 us, max 0.84 ms (while the working set fits the cache; see section 3 for 32 MB) |
| Cold whole 3 MB read from the NAS-backed zip | 6-41 ms for the 20 largest distinct files (2.9-3.5 MB: 16-36 ms; worst, the 4.0 MB entry with 2.5 MB deflated: 41 ms in the final run, 97 ms in the first run) against a bar of 10 s |
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

### Defaults changed to a 32 MB cache and a 192 MB heap limit

By ruling (the board has 491 MB), the defaults are now `-cache-mb 32` (was 64) and
`-mem-limit-mb 192` (was 256). Re-measured on SD.zip with the new defaults:

| | 64 MB cache (before) | 32 MB cache (now) |
| --- | --- | --- |
| start-up, zip page cache evicted | 0.42 s ("built in" 398 ms) | 0.35 s ("built in" 322 ms) |
| RSS after the scan (HWM / RSS) | 57.9 / 52 MB | 57.6 / 52.4 MB |
| RSS HWM after `dd bs=128k` of the whole disk | 135 MB | 99.6 MB (22-23 s, 162 MB/s, daemon CPU 17.6 + 1.8 s) |
| RSS HWM at exit after the checks | 145.5 MB | 104.6 MB |
| fsck, content (all 57,353 entries, 300 random, 20 largest, 29 non-ASCII) | clean, all equal | all equal again |

## 3. Latency

`tsshare -v -log lat.log`, SD.zip with its page cache evicted first. Tool:
`scale/fatcheck.py seq|rand`, which maps each file through the FAT, reads it
through the mount with `pread` in 16 KB chunks and joins the client timing with the
daemon's read log by offset (server latency = time inside `vhdFile.Read`; client
latency adds the kernel and FUSE round trip; "cold" = the first read of an
entry's file, which inflates the whole entry, "warm" = any later read).

Sequential: the 20 largest files with distinct (size, name) (`fatcheck.py seq` now
skips copies of the same file in other directories; the first version of the run
listed `pixeldemo.spg` three times and re-read some cached data), 1.5-4.8 MB, in
random order (seed 11), each read once in 16 KB chunks (cold) and straight
again (warm). Numbers below are from the final run with the 32 MB default (3,093
reads per pass); the 64 MB first run, with the duplicates, is in brackets.

| | median | p95 | p99 | max (us) |
| --- | --- | --- | --- | --- |
| cold, client | 15 (15) | 35 (36) | 69 (85) | 36,641 (91,947) |
| cold, server (log) | 1 (1) | 2 (4) | 19 (14) | 36,586 (91,886) |
| warm, client | 13 (14) | 24 (28) | 35 (51) | 400 (838) |
| warm, server (log) | 0 (1) | 1 (5) | 3 (10) | 37 (47) |

Each cold file's single slow request is its first read; the rest of its chunks run
at warm speed. Whole-file wall time, cold: 6-41 ms for all 20 (3.2 MB
demorama: 35-36 ms, 3.44 MB Ygor 21 ms, 4.79 MB Sword of Darkness 27 ms,
4.0 MB ts_frac3.spg with 2.5 MB deflated 41 ms; the first run measured 97 ms for
it); warm: 1-4 ms. The largest compressed entry is 2.5 MB and all stored entries
are under 8 KB, so the NFS-heavy cases are covered. **A cold 3 MB read takes
about 15-40 ms (bar: under about 10 s).**

Random: 3,000 reads of 16 KB at random 512-byte-aligned offsets inside the extents
of 300 random files (>= 16 KB), random file order (seed 5), 300 first touches:

| | median | p95 | p99 | max (us) |
| --- | --- | --- | --- | --- |
| cold (first touch of a file), client | 2,316 | 12,173 | 18,245 | 24,625 |
| cold, server | 2,244 | 12,084 | 18,123 | 24,554 |
| warm, client | 18 | 51 | 117 | 711 |
| warm, server | 2 | 7 | 13 | 91 |

With the 64 MB cache the warm reads were 2-3 orders of magnitude under the 1 ms bar.
With the 32 MB default, the same run (300 files, 3,000 reads) gives cold median
2,019 us, p95 8,448, p99 14,287, max 29,103 us, and "warm" median 36 us, p95 2,468,
p99 4,181, max 5,762 us (server p99 4,129): the 300-file working set is larger than
32 MB, so about 5-10% of the re-touches find their entry evicted and re-inflate it
(2-6 ms), which is a cache miss, not a slow warm read. With a working set that fits
(100 files, `rand ... 3000 100 5`): cold median 2,153 us, max 9,757 us; warm median
14 us, p95 29, p99 45, max 77 us (server p99 7 us). No latency fix was made.

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

5. (Fix round 1) Accounting made honest: a pinned entry that is evicted keeps
   counting (`deadBytes`) until its last reader releases it, eviction compares
   `total+deadBytes` with the budget and always terminates (it stops when only the
   newest entry is left), so memory is the budget plus the bytes pinned by in-flight
   readers and the newest entry (comment in `cache.go`; `TestCacheBudgetPinned`
   asserts it, and the cache's own `peak`, under `-race`). The previous version
   decremented `total` at eviction and a stress run with a 4 MB budget peaked at
   8.2 MB. `readStream` (entries larger than the cache budget) keeps a 256 KB
   window and an open inflater per entry for the life of the daemon; SD.zip has
   no such entry (largest 4.8 MB).
6. (Fix round 1) Defaults lowered to a 32 MB cache and `-mem-limit-mb 192`.

Result (at the first 64 MB default): 135 MB after a full read (was 266 MB), 58 MB after the scan (was 89 MB),
throughput 170 MB/s (was 126 MB/s: less GC work).

## 4b. The unexplained `go test -race` failure

One `go test -race -count=1 .` run failed right after the first commit; its output
was not captured, and it passed in 4 further race runs and 8 plain runs (and, for
the reviewer, 5 runs under load of 433 s each). The most likely culprits are the
tests with fixed timing: `TestSignalDuringBuild` (it slept a fixed 500 ms and
then expected the signal handlers to be installed and the exit within 3 s, which
under `-race` and a loaded machine can fail with the default SIGTERM action),
and, less likely, the daemon shutdown tests that use 300 ms negative waits and
`TestCacheLoadPanic` / `TestCacheLeaderFailureRetry` (100 ms for waiters to
queue; a late waiter only weakens the test). `TestSignalDuringBuild` now waits for
the daemon's `source:` line (printed after the handlers are installed and before
the build delay) through a mutex-protected buffer and allows 10 s to cancel. The
cause of the one failure remains unproven.

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
python3 -I scale/fatcheck.py seq  ~/scratch/mnt/share.vhd run.log 20 11   # 20 distinct largest files
python3 -I scale/fatcheck.py rand ~/scratch/mnt/share.vhd run.log 3000 300 5
dd if=mnt/share.vhd of=part.img bs=1M iflag=skip_bytes,count_bytes skip=65536 count=$((SECTORS*512)); fsck.vfat -n part.img
```

`SECTORS` is the partition size in the MBR (bytes 458-461 of `share.vhd`).

## 8. Warnings produced by the real zip

None. The run printed `0 warnings`.
