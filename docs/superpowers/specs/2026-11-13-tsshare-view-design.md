# tsshare view mode: pick a zip or the shared folder from the core's OSD

Status: design, 2026-11-13. Extends `tools/tsshare` (built and board-verified, see `tools/tsshare/docs/BOARD.md`).

## Intent
The owner wants to choose what appears as TSConf's card #2 from the core's OSD ("Mount SD2 image"), without typing and without fake `.vhd` files: pick a **zip** from a folder, or mount **one fixed shared folder** (`/media/fat/games/TSConf/shared`). No change to Main_MiSTer.

## How it works (decided with the owner)
- The OSD file picker is Main_MiSTer's: it lists files with the extensions in the core's `CONF_STR` and mounts the chosen file as a raw SD image. So the daemon presents a browsable **view** (FUSE tree) in which the chosen file's *content* is the virtual FAT disk.
- **View mount point:** `/media/fat/games/TSConf/share` (inside the picker's starting folder `games/TSConf`; `games` is a CIFS mount of the NAS, FUSE over a CIFS subfolder was proven in the spike).
- **Zip root:** `/media/fat/games/TSConf` (the owner's answer). The view mirrors its directory tree: sub-directories as directories, every `*.zip` (any case) as a regular file with **the same name** whose content is the virtual disk built from that zip. Nothing else is shown (the picker filters anyway). The view must **exclude its own mount point** (`share`) and the shared folder directory (`shared`, served separately) to avoid recursion.
- **Fixed folder:** the view root also contains one file `shared.vhd`: the virtual disk built from `/media/fat/games/TSConf/shared` (the owner creates/fills the folder; the daemon creates it if missing). The picker needs an extension; `.vhd` is the existing filter. The folder is **rescanned on every open**, so changes appear after the core is reset/remounted.
- **Core change (cosmetic but required to list zips):** `CONF_STR` entry `SC1,VHD,Mount SD2 image;` becomes `SC1,VHDZIP,Mount SD2 image;` (extension groups of three letters), so the picker lists `.vhd` and `.zip`. A rebuild (about 10 minutes), a timing check against the existing build and a board test. No RTL change, `VDNUM` stays 2.
- A real `.zip` picked **outside** the view (any other folder under games/TSConf) is mounted raw by Main_MiSTer: the core sees zip bytes, WC finds no card. Harmless; documented. Browse via `share/`.
- Start/stop remains manual (Scripts menu); the old single-source mode (`-src`, mount with `share.vhd`) stays for compatibility.

## Goals
- Pick a zip or `shared.vhd` in the OSD, reset happens automatically, Wild Commander shows the content as the second card.
- Several sources can be opened one after another without restarting the daemon; memory stays near one volume (about 50 MB for the 57k-entry zip) plus a small cache.
- Main_MiSTer is never stuck by the daemon: every request is answered; a failing build gives an error for that open only.
- The first open of a big zip may pause the OSD while the zip is scanned (17 s for the 57k-entry zip on the board); documented and, if cheap, shortened by an index cache.

## Non-goals
Selecting folders other than `shared`; rescanning while an image stays mounted; writes; changes to Main_MiSTer or the RTL (apart from the extension list string).

## Decisions of mine (owner can veto)
- **Volumes are built on `Open`** (not on listing), one per distinct zip, shared by all handles of the same file through a reference count. At most 2 volumes alive; an idle one is kept for a short time (default 10 minutes, then freed) so re-picking the same zip is instant; opening a third source while two are in use fails that open with EBUSY.
- **Attributes:** zip entries report the real zip size until built, the volume size afterwards, with attribute caching disabled for these nodes so `fstat` after `open` returns the built size. Main_MiSTer's size query must see the volume size (check this on the board).
- **Directory listing** of the mirrored directories is read from the real directory with a short cache (a few seconds); hidden entries (`.name`) are skipped; symlinked directories are not followed.
- **Names** shown in the view are the real names; the picker handles long names.
- `shared.vhd` is rebuilt on every `open` when no handle is open; with open handles it is reused.
- Startup scans nothing (fast start): the daemon mounts immediately; builds happen on open.
- Safety rules from the existing daemon carry over: never hang, recover panics, `-max-entries` and memory limits per volume, shutdown behaviour, strict exit codes.

## Left to the builder
FUSE node API shape (go-fuse v2.11 `fs` package), index cache format if done, test fixtures, flag names (suggested: `-view <zip-root>`, `-shared <folder>`, existing `-mount`), start script settings (`ZIPROOT`, `SHARED`, `MOUNT=/media/fat/games/TSConf/share`).

## Proof
Unit and FUSE tests on the host (mirror listing with exclusions, zip content via `fsck` + `mcopy`, concurrency on opening the same and different sources, build failure behaviour, rescan on reopen); then the core rebuild and a board test with the OSD: pick a zip, pick `shared.vhd`, switch sources, check WC and the OSD pause.
