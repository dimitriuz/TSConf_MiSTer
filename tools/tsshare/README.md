# tsshare

`tsshare` serves your TSConf collection as read-only virtual SD cards for the
TSConf core's second SD slot. It is a static Go daemon that mounts a FUSE
filesystem next to your games. The default is **view mode**: the mount shows the
folders under `games/TSConf`, every zip as `<name>.vhd` (so `SD.zip` is `SD.vhd`),
and `shared.vhd`, a disk built from the folder `games/TSConf/shared`. Main_MiSTer
mounts one of these files as "SD2 image", and the core sees a disk with an MBR and
one FAT32 partition holding the zip's (or folder's) files: Wild Commander lists it
like any drive, long names included. The older **single mode** (`MODE=single`)
serves one folder or zip as one file, `share.vhd`.

Why: large collections (10+ GB, tens of thousands of files, zips) live on a
NAS the MiSTer already mounts. Copying them to the SD card or building an image
by hand is slow and goes stale. With tsshare the "image" is built in memory
from a scan of the tree (0.4 s for the real 57,353-file zip on the host; 16.5 s on the
board, measured 2026-11-13, see docs/BOARD.md), and file data is read from the source
only when the core asks for it. No change to Main_MiSTer, `sys/` or the RTL.

Status: single mode and view mode are both tested on the build host and on the
owner's MiSTer with the real 1.4 GB zip (2026-11-13 and 2026-11-14, results in
[docs/BOARD.md](docs/BOARD.md)). See "Verified and not verified".

## Quick start on the board (view mode)

Requirements: the dual-VHD core build (the TSConf core with a second SD slot,
for example `TSConf_dualsd_20261113`) is needed for "Mount SD2 image"; and the
folder `/media/fat/games/TSConf` must already exist (the start script refuses
otherwise, it creates only `shared` and `share` inside it).

1. Copy three files to the SD card (the layout follows the Companion Remote
   daemon, `Scripts/.config/<name>/`):

   | From | To |
   | --- | --- |
   | `tsshare` (ARM build, see "Building") | `/media/fat/Scripts/.config/tsshare/tsshare` (chmod +x) |
   | `scripts/tsshare_start.sh` | `/media/fat/Scripts/tsshare_start.sh` |
   | `scripts/tsshare_stop.sh` | `/media/fat/Scripts/tsshare_stop.sh` |

2. In the Scripts menu run `tsshare_start.sh`. It views `ZIPROOT`
   (`/media/fat/games/TSConf`), serves the folder `SHARED`
   (`/media/fat/games/TSConf/shared`; by default `$ZIPROOT/shared`) as `shared.vhd`, and mounts at `MOUNT`
   (`/media/fat/games/TSConf/share`). It creates `shared` and `share` if they are
   missing (one line each) and prints `View ready: pick in the OSD` after about a
   second: the daemon scans nothing at start. From a shell you can pass another
   zip root, which replaces `ZIPROOT`: `sh /media/fat/Scripts/tsshare_start.sh
   /path/to/root`. `SHARED` then defaults to `/path/to/root/shared`, but the
   default `MOUNT` stays `/media/fat/games/TSConf/share` unless you give it. All
   three paths must be absolute. The script checks them (and that `MOUNT` is not
   inside `SHARED`, `SHARED` not inside `MOUNT`, `ZIPROOT` not inside `MOUNT`)
   before it creates anything. If `MOUNT` already holds files it prints
   `Note: files in MOUNT are hidden while mounted` and goes on.
3. Load the TSConf core, open the OSD, **Mount SD2 image**. The picker starts in
   `games/TSConf` and its folder tree mirrors that folder: go into `share` and pick
   `<zip name>.vhd` (a zip in a subfolder appears under the same subfolder) or
   `shared.vhd`, then reset the core. Wild Commander shows the disk as the SD2
   card (in the single-mode board test it was `6:\`).
4. When done: eject the image in the OSD (or reset the core), then run
   `tsshare_stop.sh`. It finds the daemon by its own command line (the pid
   file, else a scan of `/proc` for a process named `tsshare` with `-mount`),
   so it works whatever `MODE` or `MOUNT` it is run with, and prints
   `Found the daemon mounted at:` when that mount differs from its default.
   It unmounts only a `fuse.tsshare` mount at exactly the daemon's `-mount`.

### Using the shares (what you do day to day)

- **Start once** (Scripts menu, `tsshare_start.sh`) and leave it running. Putting
  new zips under `games/TSConf` needs no restart: the picker lists the folder
  live, and a zip is only scanned when you pick it.
- **Pick a source:** OSD, **Mount SD2 image**, folder `share`, then `<zip name>`
  (the picker hides `.vhd`) or `shared`. The first pick of a big zip pauses the
  OSD for about 10 s (1.4 GB, 57,353 files, measured on the board); later picks
  of the same zip are instant while it stays cached (10 minutes idle, then it is
  released). `shared` (the folder `games/TSConf/shared`) is rescanned on every
  pick, so drop files into that folder and pick it again to see them.
- **Use it in Wild Commander:** `Alt+F2` (right panel) or `Alt+F1` (left), choose
  `SD2: Z-Controller`. Browse, open `.trd` disks, play tunes. Everything is
  read-only: a write ends with "Not Enough Space!".
- **Switch sources** by picking another `.vhd` in the same OSD entry; the previous
  one is closed. At most 2 volumes stay alive (`-max-volumes`).
- **After a core reload** the OSD remembers the last SD2 image. If the daemon is
  running at that moment, the core mounts it again by itself (a zip that is no
  longer cached is rebuilt, about 10 s). If the daemon is not running, SD2 stays
  empty (Wild Commander says "Device Not Found!"); nothing hangs. Start the daemon
  and pick the image again.
- **Stop:** reset the core or load another core first, then `tsshare_stop.sh`
  (1-2 s). With an image still mounted it takes about 15 s, because the first
  TERM cannot unmount a busy mount and the script has to send a second one.

### Single mode (the alternative)

`MODE=single` keeps the old behaviour: one folder or zip, one file `share.vhd`,
mounted at `/media/fat/tsshare`. Set it for both scripts (environment or the
settings block):

```
MODE=single SRC=/path/to/folder-or-zip sh /media/fat/Scripts/tsshare_start.sh
sh /media/fat/Scripts/tsshare_stop.sh
```

The first argument of `tsshare_start.sh` is `SRC` in this mode.
`tsshare_stop.sh` needs no `MODE`. In the OSD:
Mount SD2 image, go to `tsshare`, pick `share`. The script then prints the
geometry and the warning count.

### Mount point and safety

The default view mount point sits inside the NAS-backed `games/TSConf` folder as
an empty directory. If you change `MOUNT`, any path works (spaces included; the
scripts compare against the escaped form in `/proc/mounts`), but keep it on an
empty directory that is not inside `SHARED`. The scripts only unmount a mount at
`MOUNT` whose filesystem type is `fuse.tsshare`; any other filesystem mounted
there makes the start script refuse, and the stop script leave it alone.

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

Ctrl-C in the start script (while it waits for the mount) only stops the
script: the daemon keeps starting; run `tsshare_stop.sh` to stop it.

The scripts create only `MOUNT` and, in view mode, `SHARED`, plus the files
under `Scripts/.config/tsshare/` (`tsshare.log`, `tsshare.pid`, and the binary
you put there). They edit no other file; they never touch `user-startup.sh` or
other cores' files.

Settings (environment variables or the block at the top of
`tsshare_start.sh`): `MODE` (`view`), `ZIPROOT`, `SHARED` (default `$ZIPROOT/shared`), `MOUNT` (view:
`/media/fat/games/TSConf/share`, single: `/media/fat/tsshare`), `SRC` (single
mode), `BIN`, `CACHE_MB` (32), `LABEL` (`TSSHARE`). Test hooks, used by
`scripts/test_scripts.sh`: `CONF_DIR`, `LOG`, `PIDFILE`, `WAIT_SECS`, `MOUNTS_FILE`, `SCAN_PREFIX`. The start
script waits up to 90 s for the mount and `shared.vhd` (`share.vhd` in single
mode), and stops early with the log tail if the daemon dies.

## Why `X.vhd` and not `X.zip`

The zips are shown as `.vhd` because Main_MiSTer cannot pick a `.zip` as a disk.
Its file picker turns every `*.zip` into a folder to browse into: the check is
hardcoded in `file_io.cpp` (around lines 1781 to 1787 and 198 to 209) and
`menu.cpp` (around line 2696) in Main_MiSTer commit 8329c5a. Any path that
contains `.zip` is opened through Main's own zip layer, so a zip can never be
picked as a disk under its own name by any core, and no core change helps. The
view therefore shows `SD.zip` as `SD.vhd`, a name Main picks like any image. For
the same reason names containing `.zip` are not shown in the view at all (see
below). A real `.zip` file anywhere else in the picker is a folder (Main's own zip
browsing), never a disk.

## What view mode shows

- Only folders, zips (as `.vhd`) and `shared.vhd` in the root of the mount. Other
  files are not listed.
- Hidden: names starting with a dot, symlinked folders, names containing `.zip`
  (an `a.zip.bak` or a folder `x.zip`), the mount point itself and the shared
  folder (which appears only as `shared.vhd`).
- If two entries would get the same name, the later one gets ` (2)`.
- `shared.vhd` is a fixed name in the root: a folder with that name is hidden, and a
  zip that would map to it gets the ` (2)` suffix.
- The `shared` folder is rescanned each time it is opened (after the last handle
  closed). A zip is rebuilt when its size or modification time changed.

## Cost in view mode

- Nothing is scanned at start. The first open of a zip scans it while the OSD
  waits: 17 s for the 57,353-entry zip on the board, 0.4 s on the host. The
  volume is kept afterwards: reopening within the idle time (10 minutes by
  default, `-idle-min`) is instant. After that the memory is released.
- At most `-max-volumes` sources (default 2, 1 to 3) are alive at once. An idle
  volume is evicted on demand to make room, so opening a new source while the
  old one is still mounted works (one leased plus one being built). An open
  fails with `EBUSY` (the OSD shows an error) only when that many sources are
  leased (held open) or being built.
- Memory per source: about 45 to 50 MB of tree and extent tables for the big zip,
  plus a zip cache of up to `-cache-mb` (32 MB by default) of inflated entries
  while the image is open. The cache is dropped when the last handle of a source
  closes; the tree stays for the idle time, so a quick reopen is still instant
  (entries are inflated again on demand). Worst case with the defaults: two
  volumes of about 47 MB, both being read at once, plus two caches of at most
  32 MB each, about 158 MB (below the 192 MB soft limit); typically one volume
  is in use and the total stays near 80 MB.
  A small zip or folder costs little. Peak memory in view mode on the board has
  not been measured.

## Known limits of view mode

- The picker shows every non-hidden folder under `games/TSConf`, not only the ones
  that hold zips.
- Directory listings can be up to 5 s stale.
- If a zip is rewritten while an image of it is open, reads fail with an error
  instead of returning wrong data. Eject, and open the image again.

## How it works

1. One scan of the source builds an in-memory tree (name, size, mtime). (In
   view mode this happens per zip or for the shared folder, when it is first
   opened; items 2 to 9 then apply to each volume built that way.)
2. A virtual disk is laid out from it: MBR, one FAT32 partition (type 0x0C)
   at sector 128, 2 FATs, no stored FAT table.
3. Allocation is contiguous and deterministic: root directory, then each
   directory, then each file's clusters in sorted order. A FAT sector is
   computed on demand from the extent table; directory sectors are generated
   from the tree (long names as UTF-16 entries plus an 8.3 alias).
4. Reads of `share.vhd` (or `<name>.vhd`, `shared.vhd`) are answered from those pieces; file data comes from
   the source (a directory file, a stored zip entry at its offset, or a
   deflated zip entry inflated into an in-memory LRU).
5. Folder sources keep a small LRU of open file descriptors (`-fd-limit`).
6. The `.vhd` files are mode 0444 and opened with direct I/O (one source read per
   request, no kernel readahead); writes return EROFS.
7. The daemon answers every request it can: errors become EIO and panics are
   caught per request. It has no per-read deadline, though. A source that is
   slow or stuck (an NFS hard mount, a CIFS reconnect) stalls the `ReadAt`, so
   Main_MiSTer's read blocks and the OSD freezes until the share answers;
   reading a zip entry larger than the cache backwards re-inflates it from the
   start. Mitigations: keep the source on local storage when possible, the
   in-memory cache (`-cache-mb`), and the abort recipe below. A per-read
   timeout is not implemented.
8. SIGINT/SIGTERM unmount (lazily if busy); a second signal forces a lazy
   unmount and exit.
9. The Go heap has a soft limit and a GC setting tuned for the board's RAM.
10. In single mode there is no rescan while mounted (see below); in view mode
    a volume is rebuilt after its idle time or when its zip changed.

## Flags

```
tsshare -src <dir|zip> -mount <dir> [options]               single mode
tsshare -view <zip-root> [-shared <dir>] -mount <dir> [...] view mode
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-src` | required (or `-view`) | folder or `.zip` to share (single mode) |
| `-view` | none | view mode: root folder whose zips appear as `<name>.vhd`; excludes `-src` |
| `-shared` | `<view>/shared` | view mode: folder shown as `shared.vhd`; created if missing; the mount point must not lie inside it |
| `-max-volumes` | 2 | view mode: volumes alive at once (leased or being built), 1 to 3; more means more memory |
| `-idle-min` | 10 | view mode: minutes an unused volume is kept; 1 to 1440 |
| `-mount` | required | existing directory to mount on |
| `-label` | `TSSHARE` | FAT volume label |
| `-cache-mb` | 32 | zip: MiB of inflated entries kept in memory; 1 to 256, 0 means the default 32 |
| `-max-entries` | 250000 | refuse (exit 2, nothing mounted) a source with more files and directories than this; 0 = no limit |
| `-fd-limit` | 64 | folder: open file descriptors kept |
| `-log` | none | file for warnings and (with `-v`) the read log; truncated at start |
| `-v` | off | one line per read in the `-log` file (needs `-log`) |
| `-mem-limit-mb` | 192 | soft Go heap limit in MiB (0 = off) |

Progress and the startup summary (`tree:`, `volume:`, `N warnings`, `warn:`
lines, `serving ...`) go to stderr; `tsshare_start.sh` redirects that to
`tsshare.log`.

## Geometry rules

- MBR, one FAT32 partition type 0x0C starting at sector 128 (as the owner's
  `boot.vhd`), 512-byte sectors, 2 FATs, 32 reserved sectors (padded so the
  data area starts on a cluster boundary, as on the owner's card).
- Clusters are 4 KB (8 sectors) whenever the content fits. FAT32 needs at
  least 65525 clusters, so a source under about 256 MB is padded with free
  clusters (virtual zeros, computed on demand: an empty source gives a
  257 MiB virtual disk and costs no memory). Larger clusters (up to 64
  sectors) are used only if 4 KB would exceed the FAT32 maximum cluster count.
  Only 4 KB is known to work on the board.
- A source needing more clusters than FAT32 allows is refused at start.
- The disk is the partition rounded up to 1 MiB (under 1 MiB of slack); free
  space is none to speak of.
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

## No rescan while mounted (single mode)

In view mode a volume is built on the first open and kept for the idle time (see
above). In single mode the layout is built once at start. Changes to the source while the daemon runs
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
On the board (single mode, [docs/BOARD.md](docs/BOARD.md)) VmHWM was 42.9 MB
after the scan and 47.2 MB after about 15 minutes of use. Memory in view mode
on the board is not measured yet. View mode keeps up to `-max-volumes` (default
2) such trees alive at once, and frees a volume's zip cache when its last handle
closes (see "Cost in view mode").

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
umount -l <MOUNT>     # /media/fat/games/TSConf/share, or /media/fat/tsshare in single mode
```

`tsshare_start.sh` mounts fusectl itself if it is not mounted. After `kill -9`
the mount stays and reports `Transport endpoint is not connected`; clear it
with `umount -l <MOUNT>` (`tsshare_start.sh` and `tsshare_stop.sh`
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

View mode on the host: the Go unit and FUSE tests, and a script dry run
(`scripts/test_scripts.sh`: start, `shared.vhd` and `<name>.vhd` present, no entry
containing `.zip`, second start refused, stop unmounts, a missing zip root
refused).

View mode on the owner's board (2026-11-14, details in [docs/BOARD.md](docs/BOARD.md)):
the picker lists `share` and the `.vhd` names; the 1.4 GB zip builds in 10 s on the
first pick and Wild Commander lists it and plays a MOD from it; `shared.vhd`
builds in milliseconds and shows its files; switching zip -> `shared` -> zip and
back works (the zip is reused from the cache, no rebuild) and reads still work;
peak daemon RSS 43.7 MB with both volumes alive; a write attempt fails with "Not
Enough Space!"; stop with an image mounted takes about 15 s and is clean; after a
core reload the remembered SD2 image is re-mounted when the daemon is running and
stays empty (no hang) when it is not. Not measured on the board: the 10-minute
idle release, picker listing time with many zips, and the `-max-volumes` limit
with a third source.

Verified on the owner's board (2026-11-13, single mode, details in [docs/BOARD.md](docs/BOARD.md)):
the root `DirectMount` path works (`/proc/mounts` shows `fuse.tsshare`); the scripts
run under BusyBox `ash`; start-up 16.5 s for the real zip, 43 to 47 MB peak RSS;
the OSD picker reaches `/media/fat/tsshare` and mounts `share`; Wild Commander lists
the whole tree with long names; a PT3 tune plays in the Wild Player plugin, the
next track loads on Space and the player moves on to the next file by itself at
the end of a tune; a `.trd` mounts and boots in TR-DOS; a write attempt from WC
("make directory") fails cleanly with "Not Enough Space!"; stopping the daemon
while the image is still mounted does not freeze the OSD or the core (WC then
shows an empty directory); five files read through a Linux loop mount on the
board matched the zip byte for byte.

NOT verified on the board:

- `.spg`, `.scl` and `.vgm` files, and names with non-Latin characters shown by WC
  (checked byte for byte on the host and with a loop mount, not on screen);
- long-running memory and CPU under heavy browsing (idle CPU was 0%, peak RSS 47 MB
  after about 15 minutes of use);
- a stalled NAS during a read;
- cluster sizes other than 4 KB and partitions other than at sector 128;
- the daemon's abort recipe with `tsshare` on the board (it was proven with the
  spike daemon).

## Known limits

- The NT lower-case flag byte is not set, so a name like `GAME.trd` gets an
  alias `GAME~1.TRD` and lower-case 8.3 names get long-name entries. Cosmetic:
  it only matters to software that reads the 8.3 name alone.
- No per-read timeout (see How it works, item 7).
- Single mode: no rescan while mounted (see above). View mode: see "Known limits
  of view mode".
- The MBR CHS fields are the standard `0xFE 0xFF 0xFF` placeholder.
- Each file or directory costs about 550 bytes of RAM on a 32-bit board
  (measured 552 B), so `-max-entries` defaults to 250,000. The daemon also sets
  its `oom_score_adj` to 1000 so that the kernel kills it before Main_MiSTer.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `shared.vhd` or `<name>.vhd` not visible in the OSD picker (single mode: `share.vhd`) | Is the daemon up (`cat /media/fat/Scripts/.config/tsshare/tsshare.pid`, `ls /media/fat/games/TSConf/share`; single mode `/media/fat/tsshare`)? Read `tsshare.log`. Open the OSD again after the start (untested on the board whether the picker caches its listing). |
| WC shows no card | Reset the core after mounting; check that the image is selected under SD2, not SD1. Check `tsshare.log` for `read error`; run with `-v -log` to see whether the core reads at all. |
| Slow start | The scan walks every entry once: slow NAS metadata makes it slow (the real zip took 0.4 s on the host). The start script waits 90 s. A folder on a slow share can take longer; start from a shell to watch the log. |
| `Transport endpoint is not connected` | The daemon died without unmounting. Run `tsshare_stop.sh` (or `umount -l <MOUNT>`). |
| Start says already running | Run `tsshare_stop.sh`, then start again. |
| A zip opens with an error in view mode | `-max-volumes` sources (default 2) are already open (`EBUSY`), or the zip changed while the image was open. Eject and try again. |
| Start refuses: MOUNT inside SHARED (or SHARED inside MOUNT, ZIPROOT inside MOUNT) | Pick a `MOUNT` that is a separate folder from the other two. |
| Start says "must be an absolute path" | `ZIPROOT`, `SHARED` and `MOUNT` must start with `/`. |
| The start message ends in `...` | Long paths are shortened to their last 52 characters; the log (`tsshare.log`) has the full text. |
| Start fails with the log tail shown | The log tail is the daemon's error (bad zip, source unreadable, mount refused). |
| Folder shows fewer files than the source | Count of `warnings` in the log; see the skip list above. |
