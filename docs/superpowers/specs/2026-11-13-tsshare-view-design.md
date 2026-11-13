# tsshare view mode: pick a zip or the shared folder from the core's OSD

Status: design rev 2, 2026-11-13 (rev 2 after the spike on Main_MiSTer's zip handling: zips cannot be shown under their own name). Extends `tools/tsshare` (built and board-verified, see `tools/tsshare/docs/BOARD.md`).

## Intent
The owner wants to choose what appears as TSConf's card #2 from the core's OSD ("Mount SD2 image"), without typing and without fake `.vhd` files: pick a **zip** from a folder, or mount **one fixed shared folder** (`/media/fat/games/TSConf/shared`). No change to Main_MiSTer.

## How it works (decided with the owner)
- The OSD file picker is Main_MiSTer's: it lists files with the extensions in the core's `CONF_STR` and mounts the chosen file as a raw SD image. So the daemon presents a browsable **view** (FUSE tree) in which the chosen file's *content* is the virtual FAT disk.
- **View mount point:** `/media/fat/games/TSConf/share` (inside the picker's starting folder `games/TSConf`; `games` is a CIFS mount of the NAS, FUSE over a CIFS subfolder was proven in the spike).
- **Zip root:** `/media/fat/games/TSConf` (the owner's answer). The view mirrors its directory tree: sub-directories as directories, and every `*.zip` (any case) as a regular file named **`<zip name without .zip>.vhd`** (`SD.zip` shows as `SD.vhd`) whose content is the virtual disk built from that zip. **Why not the zip's own name (spike, Main_MiSTer commit 8329c5a, `file_io.cpp:1781-1787`, `:198-209`, `menu.cpp:2696`):** Main_MiSTer's picker turns every `*.zip` into a folder to browse into (hardcoded, independent of the core's extension list; only a hardcoded `SCANO_NOZIP` flag set for some cores disables it), and any path containing `.zip` is opened through its own zip layer. So `SD.zip` and even `SD.zip.vhd` can never be picked as a disk by any core; the `.vhd` name is forced. Names containing `.zip` (case-insensitive) are never shown in the view (mirrored directories with such names are skipped with a log warning). Two zips that map to the same shown name (`a.zip`, `a.ZIP`) get a ` (2)` suffix before `.vhd`. Nothing else is shown (the picker filters anyway). The view must **exclude its own mount point** (`share`) and the shared folder directory (`shared`, served separately) to avoid recursion.
- **Fixed folder:** the view root also contains one file `shared.vhd`: the virtual disk built from `/media/fat/games/TSConf/shared` (the owner creates/fills the folder; the daemon creates it if missing). The picker needs an extension; `.vhd` is the existing filter. The folder is **rescanned on every open**, so changes appear after the core is reset/remounted.
- **No core change.** The picker's extension list stays `VHD`; the existing `TSConf_dualsd_20261113` core is used as is. (The earlier idea of `SC1,VHDZIP` would do nothing: zips are folders to Main_MiSTer regardless.)
- Real `.zip` files elsewhere appear in the picker as folders (Main's own zip browsing); selecting an inner `.vhd` of a zip would mount it read-only through Main's zip layer, which is unrelated to this daemon. Zips as disks are only available through `share/`.
- Start/stop remains manual (Scripts menu); the old single-source mode (`-src`, mount with `share.vhd`) stays for compatibility.

## Goals
- Pick a zip or `shared.vhd` in the OSD, reset happens automatically, Wild Commander shows the content as the second card.
- Several sources can be opened one after another without restarting the daemon; memory stays near one volume (about 50 MB for the 57k-entry zip) plus a small cache.
- Main_MiSTer is never stuck by the daemon: every request is answered; a failing build gives an error for that open only.
- The first open of a big zip may pause the OSD while the zip is scanned (17 s for the 57k-entry zip on the board); documented and, if cheap, shortened by an index cache.

## Non-goals
Selecting folders other than `shared`; rescanning while an image stays mounted; writes; changes to Main_MiSTer or the RTL (apart from the extension list string).

## Decisions of mine (owner can veto)
- **Volumes are built on `Open`** (not on listing), one per distinct source, shared by all handles of the same file through a reference count. At most 3 volumes alive; idle ones are kept for a short time (default 10 minutes, then freed) so re-picking the same zip is instant; a reopen compares the zip's size and mtime with the cached volume and rebuilds on a mismatch; opening a fourth source while three are in use fails that open with EBUSY.
- **Attributes:** zip entries report the real zip size until built, the volume size afterwards, with attribute caching disabled for these nodes so `fstat` after `open` returns the built size. Main_MiSTer's size query must see the volume size (check this on the board).
- **Directory listing** of the mirrored directories is read from the real directory with a short cache (a few seconds); hidden entries (`.name`) are skipped; symlinked directories are not followed.
- **Names** shown in the view are the real names; the picker handles long names.
- `shared.vhd` is rebuilt on every `open` when no handle is open; with open handles it is reused.
- Startup scans nothing (fast start): the daemon mounts immediately; builds happen on open.
- Safety rules from the existing daemon carry over: never hang, recover panics, `-max-entries` and memory limits per volume, shutdown behaviour, strict exit codes.

## Answers to the builder check (2026-11-13)
1. Zips are folders to Main_MiSTer: see above (names `X.vhd`, no core change). 2. No core branch work is needed. 3. `SC0` untouched. 4. If Main remembers a path under `share/` while the daemon is not running, the open fails and the card stays unmounted (to be confirmed on the board); the user starts the daemon and re-picks. 5. Main holds the file open while mounted; open-before-close order is unknown, hence the limit of 3 volumes. 6. Reopen compares size and mtime. 7. All non-hidden directories are mirrored (no recursive zip search on the NAS). 8. The existing scripts gain a view mode: if `ZIPROOT` is set the start script runs `tsshare -view ZIPROOT -shared SHARED -mount MOUNT`; the script creates `MOUNT` and `SHARED` if missing (owner approved creating under `games/TSConf`); the mount-point checks stay (`fuse.tsshare` fstype).

## Left to the builder
FUSE node API shape (go-fuse v2.11 `fs` package), index cache format if done, test fixtures, flag names (suggested: `-view <zip-root>`, `-shared <folder>`, existing `-mount`), start script settings (`ZIPROOT`, `SHARED`, `MOUNT=/media/fat/games/TSConf/share`).

## Proof
Unit and FUSE tests on the host (mirror listing with exclusions, zip content via `fsck` + `mcopy`, concurrency on opening the same and different sources, build failure behaviour, rescan on reopen); then the core rebuild and a board test with the OSD: pick a zip, pick `shared.vhd`, switch sources, check WC and the OSD pause.
