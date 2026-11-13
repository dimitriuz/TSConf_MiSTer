# tsshare: a folder or zip as a read-only virtual SD card for TSConf

Status: design for owner review, 2026-11-13. Builds on the FUSE spike (`spikes/fuse-vhd/`, `RESULTS.md`) and `docs/TASK-share.md`.

## Intent and why
The owner keeps large retro collections (10+ GB, thousands of files, and zips) on a NAS that the MiSTer already mounts. On TSConf they want to browse a folder or zip in Wild Commander (WC) like any drive, open a file, and have players behave as with a local folder (the music player plays the next file on its own). They do not want to copy collections onto the SD card, rebuild images when the folder changes, edit Main_MiSTer, or touch other cores' files or `user-startup.sh`. The spike proved the route: Main_MiSTer mounts a file as SD image 2; if that file is served by a FUSE daemon, the daemon can synthesize a FAT32 disk on demand.

## What gets built
`tsshare`, a static armv7 Linux daemon in Go, that:
1. takes a source: a directory tree or a single `.zip` file;
2. mounts a FUSE filesystem containing one file, `share.vhd`, read-only, which looks to the core like a whole disk: an MBR with one FAT32 partition holding the source's files and folders;
3. is started and stopped by hand on the board (ssh, or a script the owner drops into the MiSTer Scripts menu), and mounted from the OSD as "Mount SD2 image".

## Goals
- Any TSConf software that reads card 2 sees ordinary folders and files, with long file names, dates and sizes. Players and file managers behave as on a real FAT32 card.
- Scales to 10+ GB trees and tens of thousands of files on a board with 491 MB RAM, without a slow start or a big image file.
- Reads are fast: warm 16 KB reads in about 1 ms or less, cold in well under 50 ms (the spike showed 50 ms per read is tolerated but 3 MB loads then take about a minute).
- Zips work as sources, decompressed on demand.
- Never hangs Main_MiSTer: every FUSE request is answered (errors are fine, hangs are not), including with a bad source.

## Non-goals
Writes (the disk is read-only; a write attempt fails), live updates while mounted, exFAT/NTFS, zips nested in folders appearing as folders, auto-start at boot, any change to Main_MiSTer, `sys/` or the core RTL.

## Anti-goals (a failure even if it technically works)
A FAT structure that makes TS-BIOS, WC or TR-DOS misbehave; silently truncated or mangled names; a listing that omits files without saying; the daemon exhausting the board's RAM; a daemon that must be power-cycled to recover.

## Decisions already made (owner or spike)
- Go, static armv7, go-fuse v2.11.0 (v2.12.x do not build on 32-bit ARM), `DirectMount` as root (the board has no `fusermount`).
- Read-only. Source is a folder or a zip.
- The core's reads are serial, at most 16 KB, overlapping and not page-aligned (spike). The daemon must keep its own read-ahead cache.
- No edits to Main_MiSTer; no use of `user-startup.sh` or `/media/fat/linux/uartmode`.
- Deployment and tests on the owner's board only with the owner present (shared board rules in `CLAUDE.md`).

## My calls (builder may refine; owner can veto)
- **Name and place:** `tools/tsshare/` in this repo, own `go.mod`, commits `share: what`, branch `tsshare`; the spike directory stays as the record.
- **Disk layout:** MBR, one FAT32 partition (type 0x0C) starting at sector 128 like the owner's `boot.vhd`. Cluster size chosen from the content so the volume has at least 65525 clusters and the FAT stays small (spc from 1 to 64 sectors); a 10 GB tree uses 4 KB clusters. The virtual disk is the partition plus a few MB of slack, with no free space to speak of.
- **Allocation is deterministic and contiguous:** root directory first, then each directory's clusters, then each file's clusters in sorted order. No FAT table is stored: a FAT sector is computed on demand from the extent table (a cluster's next is c+1 inside an extent, end-of-chain at the extent's end). FAT2 mirrors FAT1. The cluster-to-extent lookup is a binary search.
- **Directories** are synthesized on demand from an in-memory tree built by one scan at startup (name, size, mtime, children). Names: long file name entries (UTF-16) plus a generated 8.3 alias with the usual `~N` tail and collision handling; characters FAT forbids are replaced; names over 255 UTF-16 units are truncated with a log warning. Entries sort by name. Attributes: read-only (+ archive for files, directory for dirs). Dates from mtime (FAT dates start at 1980).
- **Limits handled explicitly:** files of 4 GiB or more cannot exist on FAT32 and are skipped with a warning; empty files get cluster 0; a source with more clusters than FAT32 allows is refused at startup with a clear message.
- **Data reads:** files are opened lazily with a small LRU of file descriptors (thousands of files, so no open-everything). A per-extent read-ahead window (about 256 KB) absorbs the overlapping reads. Zip entries: stored entries are read directly at an offset; deflated entries are decompressed on first access into an in-memory LRU bounded by a total budget (default 64 MB); entries larger than the budget stream from the start with a window and restart on a backward seek.
- **No rescan while mounted.** To pick up changes, stop the daemon, unmount, restart and remount the image from the OSD (a rescan would change the layout under the core).
- **Interface:** `tsshare -src <dir|zip> -mount <dir> [-label NAME] [-cache-mb N] [-log file] [-v]`. Logs: one line per start/stop and warnings; `-v` adds a per-read log in the spike's format. Same shutdown behaviour as the spike (two-stage signal handling, lazy unmount on a second signal) and the same abort recipe in the README.
- **Safety:** recover from a panic per request and answer EIO; a source file that vanishes or errors returns EIO for that read, never a hang; reads past the end of the virtual disk return zeros as in the spike.
- **Reuse:** start from `spikes/fuse-vhd/main.go` (mount, signals, log writer, read path).

## How it is proven
- **Unit level:** FAT geometry, extent table, directory entry encoding (LFN checksums, 8.3 aliases and collisions, dates), FAT sector computation, zip entry cache.
- **Whole-volume check on the build host (no root):** mount the daemon on a test tree, copy `share.vhd` out (or read it directly) and run `fsck.vfat -n` on the partition (extracted with `dd`) and `mdir`/`mcopy -i share.vhd@@65536` against the source tree: every file's bytes must match its source; no fsck errors. Do this for a small mixed tree (unicode names, collisions, empty files, deep nesting), for a zip, and for a scale tree (about 12 GB of sparse files, 5,000 files, 300 directories).
- **Performance on the host:** warm and cold read latency histograms; memory use at scale.
- **On the board, owner present:** the spike's sequence with a real folder: the OSD mounts the file, WC lists the tree, a `.trd`, an `.spg` and a `.vgm` run, the music player auto-advances, a zip source works, the abort drill and clean shutdown work, and the board stays responsive at 50 ms-slow sources.

## Questions from the builder check, resolved (2026-11-13)
1. **Disks above 4 GiB.** Verified from the owner's working card: `boot.vhd` is 5 GiB (a 17 GB sparse `boot_newcore.vhd` is also in use), the core runs SD in SDHC mode (`sd_card .sdhc(1)`) and Main_MiSTer takes a 64-bit image size. Disks above 4 GiB already work; no size cap is needed below the FAT32 limits. The scale test on the board still uses a 10+ GB tree.
2. **Layout accepted by the on-board software.** Verified from the owner's `boot.vhd`: MBR, one FAT32 partition type 0x0C at sector 128, 512-byte sectors, 8 sectors per cluster (4 KB), 32 reserved sectors, 2 FATs, long names including Cyrillic. The spike's WC listing also showed LFN names on a FAT32 volume. So 4 KB clusters, partition at sector 128, LFN and 0x0C are known good. Other cluster sizes (for small or very large content) are unverified on the board, so the rule is: use 4 KB clusters whenever the content allows at least 65525 clusters (content of about 256 MB or more) and smaller clusters only below that; avoid clusters above 4 KB unless the 4 KB count would exceed FAT32's limit.
3. **Writes.** The spike mounted a 0444 FUSE file from the OSD without trouble (Main_MiSTer falls back to read-only), so the file is exposed as 0444. A write that reaches the daemon is rejected with EROFS. Whether a real write attempt from WC fails gracefully is untested: the board test must include one (for example a WC rename or copy onto card 2) and watch the board.
4. **Slow-source pass bar.** A cold 3 MB `.spg` from a NAS-backed source loads in under about 10 s; warm reads about 1 ms or less. Start with the simple per-extent window cache; add sequential read-ahead (an async prefetch) only if the board test misses the bar.
5. **Zip edge cases.** Same rule as large files: skip with a log warning. Names without the UTF-8 flag are decoded as CP437; entries without directory records get synthesized parent directories; duplicate paths keep the first; encrypted entries and entries of 4 GiB or more are skipped; zip64 archives with small entries work through the Go standard library.

## Left to the builder
Package layout, exact cluster-size selection rule, cache sizes and eviction details, 8.3 alias algorithm details beyond "NT style", Scripts-menu wrapper script text, test fixtures, and how to split the work into tasks.

## Open points for the owner
1. Where will the daemon and its script live on the board, and which source folder is the first real one to try (so a real collection drives the first board test)?
2. Do you want `.zip` files inside a source folder shown as folders (decided out of scope for now), or only a zip given directly as the source?
