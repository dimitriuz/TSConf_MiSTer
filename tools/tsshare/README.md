# tsshare

`tsshare` serves a folder, or a single `.zip`, as a read-only virtual SD card
for the TSConf core's second SD slot. It is a static Go daemon that mounts a
FUSE filesystem holding one file, `share.vhd`. Main_MiSTer can mount that file
as "SD2 image", and the core sees a disk with an MBR and one FAT32 partition
containing your folder or zip: Wild Commander lists it like any drive, long
names included.

Why: large collections (10+ GB, tens of thousands of files, zips) live on a
NAS the MiSTer already mounts. Copying them to the SD card or building an image
by hand is slow and goes stale. With tsshare the "image" is built in memory in
about a second from a scan of the tree, and file data is read from the source
only when the core asks for it. No change to Main_MiSTer, `sys/` or the RTL.

Status: tested on the build host only. Nothing has been run on the board yet;
see "Verified and not verified".

## Quick start on the board

1. Copy three files to the SD card (the layout follows the Companion Remote
   daemon, `Scripts/.config/<name>/`):

   | From | To |
   | --- | --- |
   | `tsshare` (ARM build, see "Building") | `/media/fat/Scripts/.config/tsshare/tsshare` (chmod +x) |
   | `scripts/tsshare_start.sh` | `/media/fat/Scripts/tsshare_start.sh` |
   | `scripts/tsshare_stop.sh` | `/media/fat/Scripts/tsshare_stop.sh` |

2. In the Scripts menu run `tsshare_start.sh`. It shares `SRC`, which defaults
   to `/media/fat/games/Spectrum/compilations/AAA/SD.zip`. Edit the settings
   block at the top of the script, or run it with one argument from a shell:
   `sh /media/fat/Scripts/tsshare_start.sh /path/to/folder-or-zip`.
   It mounts at `/media/fat/tsshare` and prints the geometry, the warning count
   and the OSD path when `share.vhd` is ready.
3. Load the TSConf core, open the OSD, **Mount SD2 image**, go to `tsshare`,
   pick `share.vhd`, then reset the core. Wild Commander shows the share as
   drive 6 or as card #2, depending on the core's mapping (not yet seen on the
   board).
4. When done: eject the image in the OSD (or reset the core), then run
   `tsshare_stop.sh`.

The default mount point lives on the SD card (`/media/fat/tsshare`). If you
change `MOUNT`, any path works (spaces included; the scripts compare against
the escaped form in `/proc/mounts`), but keep it on a local, empty directory.
The scripts only unmount a mount at `MOUNT` whose filesystem type is
`fuse.tsshare`; any other filesystem mounted there makes the start script
refuse, and the stop script leave it alone.

The binary must be named exactly `tsshare`: `tsshare_start.sh` refuses any
other `BIN` name, because both scripts recognise the daemon by that name.
`MOUNT` is used with trailing slashes removed; `/` and an empty value are
refused. Give the real path: a symlinked parent directory is not resolved, the
daemon mounts at the given path while `/proc/mounts` shows the resolved one,
and the scripts would then not see the mount.

A process is treated as the daemon only if its command name is `tsshare`, it
has `-mount <MOUNT>`, it is not the script itself or its parent, and `-mount` is a separate word (the start script always launches with `-mount <dir>`; the `-mount=<dir>` form is not recognised). The pid
file sits on the SD card and survives a power cycle: if the number in it is
dead or belongs to something else, both scripts say "Removed stale pid file"
and never signal that process.

Ctrl-C in the start script (while it waits for `share.vhd`) only stops the
script: the daemon keeps starting; run `tsshare_stop.sh` to stop it.

The scripts only create `/media/fat/tsshare` and the files under
`Scripts/.config/tsshare/` (`tsshare.log`, `tsshare.pid`, and the binary you put
there). They edit no other file; they never touch `user-startup.sh` or other
cores' files.

Settings (environment variables or the block at the top of
`tsshare_start.sh`): `SRC`, `MOUNT` (`/media/fat/tsshare`), `BIN`, `CACHE_MB`
(32), `LABEL` (`TSSHARE`). Test hooks, used by `scripts/test_scripts.sh`:
`CONF_DIR`, `LOG`, `PIDFILE`, `WAIT_SECS`. The start script waits up to 90 s
for `share.vhd`, and stops early with the log tail if the daemon dies.

## How it works

1. One scan of the source builds an in-memory tree (name, size, mtime).
2. A virtual disk is laid out from it: MBR, one FAT32 partition (type 0x0C)
   at sector 128, 2 FATs, no stored FAT table.
3. Allocation is contiguous and deterministic: root directory, then each
   directory, then each file's clusters in sorted order. A FAT sector is
   computed on demand from the extent table; directory sectors are generated
   from the tree (long names as UTF-16 entries plus an 8.3 alias).
4. Reads of `share.vhd` are answered from those pieces; file data comes from
   the source (a directory file, a stored zip entry at its offset, or a
   deflated zip entry inflated into an in-memory LRU).
5. Folder sources keep a small LRU of open file descriptors (`-fd-limit`).
6. `share.vhd` is mode 0444 and opened with direct I/O (one source read per
   request, no kernel readahead); writes return EROFS.
7. Every FUSE request is answered: errors become EIO, panics are caught per
   request, so a bad source cannot hang Main_MiSTer.
8. SIGINT/SIGTERM unmount (lazily if busy); a second signal forces a lazy
   unmount and exit.
9. The Go heap has a soft limit and a GC setting tuned for the board's RAM.
10. There is no rescan while mounted (see below).

## Flags

```
tsshare -src <dir|zip> -mount <dir> [options]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-src` | required | folder or `.zip` to share |
| `-mount` | required | existing directory to mount on |
| `-label` | `TSSHARE` | FAT volume label |
| `-cache-mb` | 32 | zip: MiB of inflated entries kept in memory |
| `-fd-limit` | 64 | folder: open file descriptors kept |
| `-log` | none | file for warnings and (with `-v`) the read log; truncated at start |
| `-v` | off | one line per read in the `-log` file (needs `-log`) |
| `-mem-limit-mb` | 192 | soft Go heap limit in MiB (0 = off) |

Progress and the startup summary (`tree:`, `volume:`, `N warnings`, `warn:`
lines, `serving share.vhd`) go to stderr; `tsshare_start.sh` redirects that to
`tsshare.log`.

## Geometry rules

- MBR, one FAT32 partition type 0x0C starting at sector 128 (as the owner's
  `boot.vhd`), 512-byte sectors, 32 reserved sectors, 2 FATs.
- Cluster size is chosen from the content: 4 KB (8 sectors) whenever that gives
  at least 65525 clusters (content of about 256 MB or more); smaller clusters
  (down to 1 sector) only below that, because FAT32 needs 65525 clusters.
  Larger clusters (up to 64 sectors) are used only if 4 KB would exceed the
  FAT32 maximum cluster count. Only 4 KB is known to work on the board.
- A source needing more clusters than FAT32 allows is refused at start.
- The disk is the partition plus a few MB of slack; free space is none to
  speak of.
- Directories: at most 65536 32-byte entries each (long names use several);
  further entries in that directory are dropped with a warning.
- Files of 4 GiB or more cannot exist on FAT32.

## What is skipped or changed, with a warning

Every item below is counted in `N warnings` and listed (first 30) in the log.

Folder source: unreadable directories; broken symlinks; symlinked
directories (symlinks to files are followed); entries that are not regular
files (sockets, devices, ...); files of 4 GiB or more.

Zip source: entries containing `..`; encrypted entries; unsupported compression
methods; stored entries with inconsistent sizes; entries of 4 GiB or more;
duplicate paths (the first is kept); a file and a directory with the same path.
Names without the UTF-8 flag are read as CP437; missing parent directories are
created.

Both: names with characters FAT forbids (`\ / : * ? " < > |`, control
characters) are changed to `_`; names over 255 UTF-16 units are cut; trailing
dots and spaces are trimmed; names that collide case-insensitively get a
changed name; the directory-entry limit above.

## No rescan while mounted

The layout is built once at start. Changes to the source while the daemon runs
are not reflected (a file that grows is clamped to its scanned size, one that
shrinks fails to read). To pick up changes: eject the image in the OSD, run
`tsshare_stop.sh`, run `tsshare_start.sh`, and mount the image again.

## Memory

Measured on the build host with the real 1.4 GB zip (57,353 files; see
[docs/PERF.md](docs/PERF.md)): about 58 MB RSS after the scan, about
100 MB (99.6 MB) after one full sequential read at `-cache-mb 32`, 105 MB at the
end of a long verification session. The defaults (`-cache-mb 32`, soft limit
`-mem-limit-mb 192`) were lowered from 64 MB after that work; the board has
491 MB RAM, shared with Main_MiSTer and the system, and `/tmp` is a RAM disk.
Peak memory on the board has not been measured.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | served and unmounted normally |
| 1 | mount failed, or a second signal forced the exit |
| 2 | bad arguments, bad mount directory, source cannot be opened, volume cannot be built |
| 130 / 143 | SIGINT / SIGTERM arrived before the mount existed |

## Aborting a hung mount (board recipe)

From the FUSE spike. Normally `tsshare_stop.sh` is enough. If the daemon hangs
or Main_MiSTer waits on it (as root):

```
mount -t fusectl none /sys/fs/fuse/connections   # if the directory is empty
ls /sys/fs/fuse/connections                      # find the number N of the mount
echo 1 > /sys/fs/fuse/connections/N/abort
umount -l /media/fat/tsshare
```

`tsshare_start.sh` mounts fusectl itself if it is not mounted. After `kill -9`
the mount stays and reports `Transport endpoint is not connected`; clear it
with `umount -l /media/fat/tsshare` (`tsshare_start.sh` and `tsshare_stop.sh`
do this for you). A second SIGTERM also forces a lazy unmount.

## Building and testing

Host build and tests (Go, FUSE tests need `/dev/fuse` and `fusermount3`):

```
cd tools/tsshare
go vet ./...
go test ./...
```

ARM build for the board (go-fuse is pinned to v2.11.0 because v2.12.x does not
build on 32-bit ARM); about 3.0 MB:

```
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o tsshare-arm .
```

Copy it to the board as `tsshare` (no suffix). Script dry run on the host
(non-root, builds the daemon, mounts through `fusermount3`, skips with a message
if FUSE is unavailable, always unmounts):

```
tools/tsshare/scripts/test_scripts.sh
```

## Verified and not verified

Verified on the build host (details and numbers in [docs/PERF.md](docs/PERF.md)):
`fsck.vfat -n` clean on the 3.4 GiB partition built from the real zip; every
file's content equals the zip's (57,353 of 57,353, md5); a 12 GB sparse scale
tree; warm and cold read latency; memory figures above; the unit and FUSE
tests; the start/stop scripts through `scripts/test_scripts.sh`
(non-root, `fusermount3`).

NOT verified on the board:

- the root `DirectMount` path (the board has no `fusermount`); the code falls
  back to `fusermount3` if the strict mount fails, which on the board will
  simply fail;
- that the OSD file picker reaches `/media/fat/tsshare` (the SD card root is
  exFAT with some CIFS-mounted folders) and mounts `share.vhd`;
- Wild Commander, players and TR-DOS reading the share; a write attempt from
  WC onto the card;
- speed and memory on the ARM Cortex-A9 and with a NAS-backed source;
- the scripts under BusyBox `ash`, `ps` and `umount` (only checked against
  `sh`/`dash` and `bash --posix`, no BusyBox or shellcheck on the host);
- cluster sizes other than 4 KB and partitions other than at sector 128.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `share.vhd` not visible in the OSD picker | Is the daemon up (`cat /media/fat/Scripts/.config/tsshare/tsshare.pid`, `ls /media/fat/tsshare`)? Read `tsshare.log`. Open the OSD again after the start (untested on the board whether the picker caches its listing). |
| WC shows no card | Reset the core after mounting; check that the image is selected under SD2, not SD1. Check `tsshare.log` for `read error`; run with `-v -log` to see whether the core reads at all. |
| Slow start | The scan walks every entry once: slow NAS metadata makes it slow (the real zip took 0.4 s on the host). The start script waits 90 s. A folder on a slow share can take longer; start from a shell to watch the log. |
| `Transport endpoint is not connected` | The daemon died without unmounting. Run `tsshare_stop.sh` (or `umount -l /media/fat/tsshare`). |
| Start says already running | Run `tsshare_stop.sh`, then start again. |
| Start fails with the log tail shown | The log tail is the daemon's error (bad zip, source unreadable, mount refused). |
| Folder shows fewer files than the source | Count of `warnings` in the log; see the skip list above. |
