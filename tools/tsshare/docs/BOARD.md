# tsshare on the MiSTer board: results (2026-11-13)

Board: MiSTer at 192.168.1.15 (armv7l, 491 MB RAM, kernel 6.18.38), core `TSConf_dualsd_20261113`
(dual SD images), daemon built from branch `tsshare` commit 335419f, source: the owner's
`SD.zip` (1.4 GB, 57,353 files, 1,034 directories, read over the board's CIFS mount of the NAS
at `/media/fat/games/Spectrum/compilations/AAA/SD.zip`). The OSD was driven remotely through the
MiSTer Companion Remote daemon and watched on the HDMI capture feed; see `~/repo/MiSTer_docs`.

## Deploy
Binary (3,014,816 bytes, md5 `322babbc...`) and both scripts copied with `scp -O` to
`/media/fat/Scripts/` and `/media/fat/Scripts/.config/tsshare/`; checksums matched; the binary ran.

## Start (`tsshare_start.sh`, run as the Scripts menu would)
| Item | Result |
|---|---|
| Script wall time to "ready" | 17-18 s |
| Daemon scan and build | `built in 16.544s` (host: 0.4 s; the 5.7 MB zip directory is read over CIFS on a Cortex-A9) |
| Geometry | 57,353 files, 1,034 dirs, 8 sectors/cluster, 912,542 clusters, 36 reserved sectors, data at partition sector 14,296, disk 3,745,513,472 bytes, 0 warnings |
| Mount | `tsshare /media/fat/tsshare fuse.tsshare rw,nosuid,nodev,relatime,user_id=0,group_id=0,max_read=131072`; root direct mount worked, `share.vhd` 3,745,513,472 bytes, mode 0444 |
| Memory | VmHWM 42.9 MB after the scan, 47.2 MB after about 15 minutes of use; board free RAM 251 MB before, 206 MB after (-45 MB); 8-10 threads; idle CPU 0% |
| `dmesg` | no FUSE messages |

## Linux-side content check (loop mount on the board: `mount -o loop,ro,offset=65536`)
Five files chosen from the zip with an independent reader on the host (`.trd`, `.pt3`, `.scl`, two `.vgm`):
md5 on the board equals the zip for all five (MATCH x5). Timings (cold, via loop mount):
655 KB `.trd` 431 ms; 251 KB `.scl` 145 ms; 4.8 MB `.vgm` 985 ms; a 2 KB file 85 ms to 1.3 s (the
first access walks the directory tree); `ls -R` of a folder tree with 33,802 entries 2.3 s.
Peak RSS after this: 47 MB.

## OSD and Wild Commander (core `TSConf_dualsd_20261113`)
1. Mount SD image: the picker starts in `games/TSConf`, goes up to `/media/fat` (HOME/END and `..` work) and
   lists `tsshare` and `tsshare-test` (the default mount point is reachable). Image 1 was a byte-identical
   copy of `vhd-example.vhd` in `/media/fat/tsshare-test/` (the owner's original was never mounted).
2. Wild Commander booted from image 1 and showed "Device Not Found" for the empty card 2 (expected), with
   `SD2: Z-Controller` in its device list.
3. Mount SD2 image: `tsshare/` contains `share` (the picker hides `.vhd`); after selecting it and the
   automatic reset, Wild Commander shows the share as **drive `6:\`** next to image 1 (`1:\wc\`), with the zip's
   27 top-level entries, long names, and dates.
4. Music: `0_Test_OmniPlayer\pt3\` lists the `.pt3` files with their long names. Enter on the first starts the
   **Wild Player plugin** (Vortex Tracker II, "[ALL] FORWARD"); Space loads the next file; a tune that ended by itself
   was followed by the next file automatically (second tune about 61 s, the player then showed the third file at 0:14).
5. Disk image: `cde.trd` (655,360 bytes) mounted to TR-DOS Drive A; **RESET with BOOT** booted it into
   Quick Commander v3.11 showing the disk's 128 files.
6. Reset from the OSD: Wild Commander came back with the share still present.
7. Write attempt: F7 (make directory) `test` on `6:\` gives **"ERROR: Not Enough Space!"**; nothing hung, the log
   stayed clean.
8. Stop with the image still mounted: `tsshare_stop.sh` warned about it; the first TERM tried a clean unmount
   (busy), then a lazy unmount; the second TERM 13 s later ended the daemon (14 s in total, exit 0, no mount
   left). The OSD opened normally afterwards and the core kept running; Wild Commander showed the opened share
   directory as empty (no freeze, no crash). Log: `got terminated`, `unmount: device or resource busy; trying
   lazy unmount`, `got terminated again, exiting`, `lazy unmount: invalid argument`.

## Cleanup
Core set back to `menu.rbf`; removed the test image copy, `/media/fat/tsshare-test`, the empty mount point and
the `fusectl` mount; the daemon binary, the log and the two scripts were left installed in
`/media/fat/Scripts/` (visible in the Scripts menu).

## Findings
- Start-up on the board is 17 s, much slower than the host (0.4 s) and than the 2-4 s estimate: it is the zip's
  central directory read over CIFS plus building 57k nodes on the Cortex-A9. Start it once and leave it running.
- Everything the owner asked for works in this first run: browsing a folder of the zip as a card, opening a
  tune that plays on to the next file by itself, and booting a disk image.
- `tsshare_stop.sh` takes about 14 s when the image is still mounted (first TERM cannot unmount a busy mount);
  eject or reset first for a quick stop.
- Process notes for future runs: the capture feed was dead because its publisher was stopped by job control and
  had to be restarted by the owner; the HDMI card is `/dev/video4`, `/dev/video0` is a webcam (see MiSTer_docs).

## Not checked
`.spg`, `.scl` and `.vgm` on the board; non-Latin names on screen; a stalled NAS; long-running load; cluster
sizes other than 4 KB; the abort recipe with `tsshare`.

# View mode on the board (2026-11-14)

> Naming: these runs used the old folder names. Since 2026-11-16 the mount window is `games/TSConf/virtualfs` (was `share`) and the real folder is `games/TSConf/share` (was `shared`, shown as `shared.vhd`, now `share.vhd`). Not re-run on the board after the rename.

Binary: HEAD b10a5b7 static ARM build (md5 `cd7dfb17…`), scripts from the same commit. Board idle (`MENU`) before and after.
Setup: `ZIPROOT=/media/fat/games/Spectrum/compilations/AAA` (`games/TSConf` holds no zips), `SHARED=/media/fat/games/TSConf/shared`
(the script created `share/` and `shared/`; the owner had approved that), core `TSConf_dualsd_20261113`, image 1 a copy of
`vhd-example.vhd`. Driven with the Companion Remote and the `/dev/video4` capture.

## Results
1. **Start:** the script reported ready in 1 s. `share/` showed `SD.vhd`, `shared.vhd` (0 bytes until first opened) and
   the zip folder's own subfolder `disks`; no raw `.zip` entries.
2. **Picker:** in the OSD, `Mount SD2 image` opens in `games/TSConf` and lists `share` and `shared`; inside `share/` it lists
   `disks`, `SD`, `shared` (the `.vhd` suffix is hidden by the picker).
3. **Big zip (1.4 GB, 57,353 files):** the first pick built the volume in 10.2 s (log: 58,387 nodes, 0 warnings), faster than
   the 17 s of single mode. Wild Commander (Alt+F2, `SD2: Z-Controller`) listed the zip root at once, opened
   `9_Music_tracker format\5.1_Mod_music_tarcks` and the Sound Player played `1.1loadersong.mod`.
4. **Switch to `shared.vhd`:** built in 9 ms, WC showed `hello.txt` (a file put there for the test). A later re-pick rebuilt it
   ("close ... replaced", "directory rescan", 8 ms), so the folder is rescanned on each open.
5. **Memory:** daemon `VmHWM` 43.6 MB after the 1.4 GB zip build and after switching to `shared`; board `free` 253 MB at that
   point (82 MB used). Two volumes were never alive at once in this run.
6. **Write attempt:** WC make-directory on the card gave `Not Enough Space!`; nothing changed, the core kept running.
7. **Stop with the image mounted:** `tsshare_stop.sh` warned, first TERM hit `unmount: device or resource busy`, the second TERM
   ended the daemon (15 s total, exit 0, no mount left, memory back to 45 MB used). The board stayed responsive.
8. **Remembered path, daemon down:** reloading the core with the daemon stopped left SD2 unmounted (the picker path is not
   re-mounted), image 1 loaded normally, `share/` was an empty plain directory; no hang. WC showed `Device Not Found!` for the
   right panel, which is the expected message for an empty card 2.

## Switch test, second run (2026-11-14, 08:38-08:42)
1. Core reload with the daemon already running: the remembered SD2 path was mounted again by itself (log: zip build at
   08:38:24, 10.3 s, no key pressed). Reloading with the daemon down (first run) left SD2 empty.
2. Picked image 1 (core resets): SD2 stayed the zip, no second build.
3. SD2 -> `shared`: 8 ms build, WC showed `hello.txt`. Daemon RSS 43.7 MB.
4. SD2 -> `SD` again: no new build line (the cached zip volume was reused), WC showed the zip root, folders opened, and a MOD
   played in the Sound Player. Board `free` 254 MB, daemon `VmHWM` 43.7 MB with both volumes alive.
5. Return to the menu core and `tsshare_stop.sh`: 1.5 s, clean (`got terminated`, both volumes `manager closed`).
The first run's "switch back" did not land on the zip because of keys sent while the WC device dialog was open; it was a
test mistake, not a daemon fault.

## Not verified
The 10-minute idle release, picker listing time with many zips, a third source against `-max-volumes`, odd file names,
EINTR paths.

## Process notes
- Companion Remote keeps a key held only while the same WebSocket connection is open, so `mister_remote.py raw` with a
  separate `down`/`up` per call loses the modifier. For Alt+F2 send down/down/up/up on one connection.
- WC's "Select Device" dialog stays open when the OSD is closed: arrow keys sent for the OSD then move its cursor. Screenshot
  before each Enter.

## Cleanup
Core set back to `menu.rbf` (`/tmp/CORENAME` = `MENU`); removed `hello.txt`, the image copy on the NAS and the test mount;
the daemon (new build), the log, the scripts and the empty `games/TSConf/share` and `shared` directories were left.
