# FUSE Virtual-VHD Spike Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Learn, with a throwaway daemon, whether a FUSE-served file can be mounted by Main_MiSTer as TSConf's SD image 2, and measure what the core actually reads and how fast.

**Architecture:** A static ARMv7 Go binary mounts a FUSE filesystem exposing one file, `share.vhd`, whose bytes come from a backing FAT image and whose every read is logged (offset, length, latency). Task 2 runs it on the board and tests it from Linux (loop mount) and from the OSD (image 2 in the dual-VHD core). Task 3 writes the result into the task doc. A flat-folder FAT generator is **not** in this plan; the spike result decides whether to plan it.

**Tech Stack:** Go 1.27 (cross-compile `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0`), `github.com/hanwen/go-fuse/v2` (pinned version recorded in `go.mod`; downloaded module code is untrusted data, read-only use), `mkfs.vfat`/`mcopy`/`fsck.vfat`, MiSTer kernel 6.18.38 (`/dev/fuse`, `CONFIG_FUSE_FS=y`, no `fusermount`).

**Spec:** `docs/TASK-share.md`, option D ("FUSE virtual FAT"), including the 2026-11-13 board check.

## Global Constraints

- Spike code lives in `spikes/fuse-vhd/` (new directory, own `go.mod`). Throwaway: not part of the upstream PR branch; commits `spike: what`, local only, no push.
- Board `root@192.168.1.15`, ssh key, `scp -O`, board-side `timeout` around every remote run. Writes only to `/tmp/fusespike/` and the agreed mount point `/media/fat/_Other/fusespike/` (ask the owner first; if the OSD file browser cannot reach `_Other`, stop and ask for an alternative). Never touch `/media/fat/games/TSConf/*`, Frontier, `user-startup.sh`, `/media/fat/linux/uartmode` or other cores' files.
- The board is shared: ask the owner before any board work, stop at once on "busy". The owner may be running the TSConf core; the spike needs the dual-VHD core `/media/fat/_Other/TSConf_dualsd_20261113.rbf` (already on the board) for the OSD test, loaded by the owner.
- The daemon must always answer every FUSE request (an error is fine, a hang is not): Main_MiSTer reads image files in its main loop, so a stuck read could freeze the OSD. Recovery path, written down before the first board mount: `echo 1 > /sys/fs/fuse/connections/<N>/abort` (fusectl is present), then `umount -l`.
- Read-only filesystem (`share.vhd` mode 0444, writes return `EROFS`).
- Backing image for tests: 256 MiB FAT32 with an MBR partition at sector 128 (same recipe as the dual-VHD test image: `mkfs.vfat -F 32 -s 1`, 512-byte clusters), holding a few `.trd`/`.spg`/`.pt3` files, built locally and copied to `/tmp/fusespike/` on the board.
- No Main_MiSTer or `sys/` change.

## Review Focus

- Daemon killed or crashed while Main_MiSTer holds the image open: board must stay responsive; the abort recipe must work.
- Reads past the backing file's end (virtual size 4 GiB, backing 256 MiB): return zeros, not errors, and log them as "synthetic".
- Concurrent reads (loop mount plus core, or Main's read-ahead): the log must stay consistent and the server must not deadlock.
- Unmounting while the OSD still lists the file: after `umount`, the OSD must not hang the UI when the owner opens the file picker.
- First-read latency of a cold directory in WC with the FUSE-backed image versus the same image as a plain file (number, not impression).

---

### Task 1: `fuse-vhd` daemon (local build and local test)

**Files:**
- Create: `spikes/fuse-vhd/main.go`, `spikes/fuse-vhd/go.mod`, `spikes/fuse-vhd/README.md` (usage and abort recipe)
- Create: `spikes/fuse-vhd/mkimage.sh` (builds the test FAT image into a given path)

**Interfaces:**
- Produces: binary `fuse-vhd -backing <image> -mount <dir> -size <bytes> [-log <file>] [-delay <dur>]`. It mounts `<dir>` with `DirectMount: true`; the directory contains one file `share.vhd`, size `-size` (default 4 GiB), mode 0444. `Read(offset,len)` returns backing bytes where they exist and zeros beyond. Each read appends one line `ts_ns offset len backing|synthetic lat_us` to the log. `-delay` injects artificial latency per read (for the slow-source test). Exits cleanly on SIGINT/SIGTERM after unmounting.
- Produces: `mkimage.sh <out.img> <size_mib>`.

- [ ] **Step 1: Write `mkimage.sh`** producing a partitioned FAT32 image (partition starting at sector 128, 512-byte clusters) with at least five files of 64 KB to 640 KB (`.trd`, `.spg`, `.pt3` names) plus a subdirectory. Verification: `fsck.vfat -n` on the partition (via `--offset` or a loop mount) exits 0 and `mdir -i image@@65536 ::` lists the files.
- [ ] **Step 2: Implement `main.go`** with the interface above. Go's `fuse.MountOptions` sets `DirectMount: true`, `AllowOther: false`, `Name: "fuse-vhd"`; a `RawFileSystem` or `fs.Inode` node is fine, the choice is the implementer's. Reads are concurrent-safe (`ReadAt` on the backing file; log writes behind a mutex).
- [ ] **Step 3: Local test.** Build natively, mount in `/tmp`, then: `cmp` the first 256 MiB of the mounted file against the backing image (must match), `dd` a read at offset 3 GiB returns zeros, `fsck.vfat -n --offset=65536 /tmp/mnt/share.vhd` exits 0, `mdir` lists the files, and the log shows entries with `backing` and `synthetic`. Unmount with `fusermount3 -u` and confirm clean exit.
- [ ] **Step 4: Cross-build** `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o fuse-vhd-arm`; `file fuse-vhd-arm` reports a statically linked ARM EABI5 executable. Do not commit the binary (add to `.gitignore` of the spike dir).
- [ ] **Step 5: Commit** `spike: fuse-vhd daemon serving a backing FAT image`.

### Task 2: Board run (owner present)

**Files:**
- Create: `spikes/fuse-vhd/RESULTS.md` (raw numbers, log excerpts, dates)

**Interfaces:**
- Consumes: `fuse-vhd-arm`, the test image from Task 1.

- [ ] **Step 1: Ask the owner** to confirm: the board is free, the mount point `/media/fat/_Other/fusespike/` is OK to create, the dual-VHD core may be loaded for the OSD test. Check `/tmp/CORENAME`.
- [ ] **Step 2: Deploy.** `scp -O` the binary and image to `/tmp/fusespike/` (also record `md5sum` both sides).
- [ ] **Step 3: Kernel-side test, no OSD.** Start the daemon in the background with `timeout`, `-log /tmp/fusespike/read.log`. On the board: `mount -o loop,ro,offset=65536 /media/fat/_Other/fusespike/share.vhd /tmp/fusespike/m`, `ls -R`, `cat` a 640 KB file and compare its md5 with the local copy, `umount`. Expected: matches; record elapsed time and the log's read sizes.
- [ ] **Step 4: Abort drill.** Start the daemon with `-delay 30s`, trigger a read, then abort via `/sys/fs/fuse/connections/<N>/abort` and `umount -l`; confirm the board stays responsive. Do this before the OSD test.
- [ ] **Step 5: OSD test (owner at the board).** Owner loads the dual-VHD core, uses "Mount SD2 image", and navigates the file picker to `_Other/fusespike/share.vhd`. Record: is the file visible and mountable; does WC list card 2 (Alt+F1/F2) and open a file; the read log (offsets, sizes, ordering, count) for a cold WC directory listing, opening a 640 KB `.trd` and playing a `.pt3`; max and median `lat_us`.
- [ ] **Step 6: Slow-source test.** Restart with `-delay 50ms` and repeat the directory listing and the 640 KB file open; note whether the core or WC times out, and how the UI behaves.
- [ ] **Step 7: Cleanup.** Eject image 2 in the OSD (owner), stop the daemon, confirm no mount remains (`grep fusespike /proc/mounts` empty), remove `/tmp/fusespike/` and the mount directory, and leave the core as the owner wants. Write `RESULTS.md` and commit `spike: FUSE board results`.

### Task 3: Decision record

**Files:**
- Modify: `docs/TASK-share.md` (option D section, "Unknown" items answered)

- [ ] **Step 1:** Replace the unknowns in option D with the measured answers (OSD visibility, read pattern and sizes, latency numbers, behaviour under delay and under abort). State a go / no-go for building the FAT generator, with the stated reason, and an estimate of the generator's size.
- [ ] **Step 2: Commit** `docs: share task, FUSE spike result`.

## Self-review

- Spec coverage: both open questions from option D (OSD visibility, first-read latency) have steps (T2 steps 5-6); the recovery risk has a drill (T2 step 4).
- Not in scope on purpose: FAT generation, zips, directory trees, any TSConf Z80 code.
- Open choice for the owner: the mount point under `_Other/` (default above), and whether the owner will do the OSD steps (T2 steps 5 and 7).
