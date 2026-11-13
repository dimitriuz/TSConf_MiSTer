# FUSE virtual-VHD spike: results (2026-11-13)

Board: MiSTer at 192.168.1.15, kernel 6.18.38-MiSTer (armv7l, 491 MB RAM), core `TSConf_dualsd_20261113`, daemon `fuse-vhd-arm` (go-fuse v2.11.0, static, `DirectMount`). Backing images were on the NAS: the board's `/media/fat/_Other` is a CIFS mount, so the latencies below include the NAS page cache and are not SD-card numbers.

## Verdict
FUSE works. The file is mounted by Main_MiSTer as SD image 2 from the OSD file picker, WC lists it as a normal drive, and real files (`.trd`, `.spg`, `.vgm`) open and run. Go/no-go for a FAT generator: **go**.

## What was measured
| Test | Result |
|---|---|
| Deploy | binary and image md5 match, binary runs on armv7 |
| Root `DirectMount` (no fusermount on the board) | works |
| Loop mount of the FUSE file on the board, `ls -R`, md5 of a 640 KB file | all OK, md5 equal to the local copy |
| SIGTERM | clean unmount, log closed, 0 dropped lines, exit 0 |
| Abort drill (`-delay 30s`, hung `dd`) | `echo 1 > /sys/fs/fuse/connections/N/abort` gives `ECONNABORTED` to the reader, board stays responsive, `umount -l` clears the mount, daemon exits when its in-flight read ends. **`fusectl` is not mounted by default**: `mount -t fusectl none /sys/fs/fuse/connections` first. |
| OSD file picker | `share.vhd` visible under `Other/fusespike*/` and mountable as "Mount SD2 image" |
| WC with image 1 = vhd-example copy, image 2 = FUSE file | both drives listed; with the real-file image (SYNCHRO.TRD 640 KB, three SPGs up to 3.2 MB, two VGMs) everything opens/runs |
| Read pattern, normal run (1323 reads, 12.3 MB over 179 s) | 16 KB reads (689) plus short 0.5-3.5 KB reads; no read-ahead (longest contiguous run = 1 read); median 43 us, p95 122 us, p99 1.3 ms, max 5.7 ms |
| `-delay 50ms` (193 reads, 1.9 MB over 77 s) | owner: "all works fine, with little delay at start". Main issues one read at a time (strictly serial, 50.4 ms each), so throughput is about 16 KB per read latency. |

## Request amplification (owner: "only demorama is much slower to start")
`demorama_TS.spg` is 3.09 MB, contiguous on disk (clusters 3149-9468, 512-byte clusters). With `-delay 50ms` loading it produced **885 FUSE requests, 8.05 MB requested (2.6x the file), over 56.8 s**; the other files (up to 0.9 MB) felt fine. The requests overlap and are not page-aligned (for example 16 KB at offset 0, then 16 KB at 512, then short reads of 1.5-3.5 KB up to the next page boundary, then 16 KB again), so the kernel page cache is not absorbing the core's sector-by-sector reads. Without the artificial delay the same load costs under a second (median request 43 us). Consequence for the real daemon: a slow source (zip entry decompression, a network file, lazy FAT synthesis) must sit behind an in-memory read-ahead cache in the daemon (for example a 256 KB window per file extent, kept while reads stay inside it), so the source is paid once per window and the overlapping requests cost microseconds. Also worth testing: `FOPEN_KEEP_CACHE`/page-cache settings in go-fuse to let the kernel absorb repeats.

## Findings that shape the real daemon
- Random-byte test files are not useful: WC plugins and TR-DOS validate content. Use real files.
- Reads are serial and 16 KB or smaller, at arbitrary offsets: a lazily synthesized FAT must answer any 512-byte-aligned range at any time, in under about 50 ms.
- A 4 GiB sparse virtual size works for Main_MiSTer and the core; reads beyond the backing image return zeros.
- Main_MiSTer only mounts what the OSD picker shows. The picker is rooted at the core's game folder and can navigate to `_Other`; the mount point must be a normal directory there.
- Driving the MiSTer OSD from outside failed: virtual uinput keyboard and virtual Xbox pad (045e:028e) are seen (F12 works) but arrows/D-pad/Enter do nothing. Owner did the OSD steps. The capture feed (`rtsp://localhost:8554/capture`) works for watching, about 2 s lag.

## Caveats
- Numbers come from a NAS-backed image; the board rebooted once during the run (daemon restarted, logs of the first run lost with `/tmp`).
- Not tested: write path (read-only by design), EIO path, concurrent clients, zips, large directories.
