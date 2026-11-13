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
