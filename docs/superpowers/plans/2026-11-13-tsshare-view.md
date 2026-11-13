# tsshare view mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tsshare -view <zip-root> -shared <folder> -mount <dir>` mounts a browsable FUSE view in which every zip under `<zip-root>` appears as `<name>.vhd` and one fixed folder appears as `shared.vhd`, each being a virtual FAT32 disk built on open, so the owner picks a zip or the shared folder in the core's existing "Mount SD2 image" OSD picker.

**Architecture:** A `Manager` builds and caches `Volume`s on demand (reference-counted leases, idle TTL, change detection, concurrency limit). A go-fuse `fs` tree mirrors the real directories, maps zip names, and serves reads from leased volumes. The existing single-source mode (`-src`, `share.vhd`) stays unchanged.

**Tech Stack:** Go 1.27, go-fuse v2.11.0 `fs` package (already used in `tools/tsshare/main.go`), existing `Source`/`Volume` code, host tests with `fusermount3`, `fsck.vfat`, mtools.

**Spec:** `docs/superpowers/specs/2026-11-13-tsshare-view-design.md` (rev 2). Existing code: `tools/tsshare/*.go` (read `main.go` `root`/`vhdFile`/`mountFS`, `source.go`, `volume.go`, `dirsource.go`, `zipsource.go`, `cache.go`).

## Global Constraints

- Code in `tools/tsshare/` (package `main`), commits `share: what`, branch `tsshare`, local only, no push. Do not touch `sys/`, `rtl/`, `TSConf.sv`, `spikes/`.
- **Names:** Main_MiSTer treats any `*.zip` as a folder and any path containing `.zip` as zipped, so the view never shows a name containing `.zip` (case-insensitive): a zip `X.zip` is shown as `X.vhd`; two zips mapping to the same shown name (case-insensitive) get ` (2)`, ` (3)` before `.vhd`; mirrored directories whose name contains `.zip` are skipped with a log line. Hidden names (`.x`) are skipped. Only directories, `*.zip` files and `shared.vhd` are shown. The view excludes its own mount point and the shared directory.
- **Defaults:** MaxVolumes 2 (was 3; superseded by `-max-volumes`, default 2, range 1..3, for memory), idle TTL 10 minutes, listing cache 5 seconds, existing flag defaults (`-cache-mb 32`, `-mem-limit-mb 192`, `-max-entries 250000`).
- **Never hang:** every FUSE request is answered; build errors fail that open only (EIO), panics recovered, `EBUSY` when `-max-volumes` volumes are leased; existing shutdown/exit-code rules unchanged. Read-only: writes return EROFS.
- **Size semantics:** before a volume is built a zip entry reports the real zip's size; after `Open` the built volume's size (attribute caching disabled for these nodes so `fstat` on the open handle returns the built size); `shared.vhd` reports the built size when open, else the last known size or 0.
- Board rules as in `CLAUDE.md` and `~/repo/MiSTer_docs`; the board test is Task 4 and needs the owner's presence.
- No core rebuild: the extension list stays `VHD` (design rev 2).

## Review Focus

- A zip modified (size or mtime) between opens must rebuild; a zip replaced while a lease is open must not corrupt the open handle's reads.
- Opening the same source from several handles/threads builds once; releasing the last handle keeps the volume idle for the TTL, then frees it (memory returns).
- `share/` inside the zip root (the view's own mount point) must never appear; symlink loops and a mount point given as a relative or symlinked path must not cause recursion.
- Names: case-twin zips, a zip named `a.zip.zip`, unicode, a directory named `foo.zip.d`, a file named `X.ZIP`, a zip inside the shared directory.
- Slow or failing underlying directory (CIFS stall, permission denied, vanished entry) gives errors/empty listings for that request only.
- Concurrency: `Release` after `Close`, double release, `Acquire` while the TTL sweeper evicts.

---

### Task 1: Volume manager

**Files:**
- Create: `tools/tsshare/manager.go`, `tools/tsshare/manager_test.go`

**Interfaces:**
- Produces:
  ```go
  type SourceSpec struct{ Path string; Zip bool } // Zip: file is a zip; else a directory
  type ManagerOptions struct {
      MaxVolumes int            // default 2 (was 3)
      IdleTTL    time.Duration  // default 10m
      Source     SourceOptions  // passed to OpenSource
      Volume     VolumeOptions  // passed to NewVolume (Label etc.)
      MaxEntries int            // per volume (0 = no limit)
      Now        func() time.Time // injectable clock (default time.Now)
      Logf       func(format string, a ...any)
  }
  func NewManager(o ManagerOptions) *Manager
  func (m *Manager) Acquire(spec SourceSpec) (*Lease, error) // builds on first use; ErrBusy if MaxVolumes volumes are leased
  func (l *Lease) Volume() *Volume
  func (l *Lease) Release()                                  // idempotent
  func (m *Manager) Sweep()                                  // evicts idle volumes past TTL (also run by a background ticker)
  func (m *Manager) Close()                                  // closes everything; later Acquire fails
  var ErrBusy = errors.New("tsshare: too many sources in use")
  ```
  Rules: key = cleaned absolute path. A zip volume is reused only if the zip's size and mtime still equal those recorded at build time, else rebuilt (old volume closed once its last lease is released). A directory volume is rebuilt on `Acquire` when it has no leases; with leases it is reused. Concurrent `Acquire`s of one key build once (singleflight) and all receive the same volume or the same error; a failed build is not cached. A panic inside the build becomes an error.

- [ ] **Step 1: Write the failing tests** in `manager_test.go` (injectable clock, small zips/dirs in `t.TempDir()`): `TestAcquireBuildsOnce` (20 goroutines, one build counted through a hook or `Logf`), `TestReleaseKeepsIdleThenSweeps` (idle until TTL, evicted after, memory handle closed: `Source.Close` called), `TestZipChangedRebuilds` (size/mtime change between acquires; an open lease keeps reading the old data correctly and the old volume closes only after its release), `TestDirRescanOnReacquire` (file added to the shared dir appears after release+reacquire, not while leased), `TestBusyAtLimit` (3 leased, 4th gives ErrBusy; after a release it works), `TestBuildErrorNotCached` (corrupt zip errors twice the same way, then succeeds after fixing the file), `TestBuildPanicIsError`, `TestDoubleReleaseAndClose` (idempotent, Release after Close safe, Acquire after Close fails), `TestMaxEntries` (refused volume gives a clear error). All with `-race`.
- [ ] **Step 2: Run** `cd tools/tsshare && go test -race -run 'Acquire|Release|ZipChanged|DirRescan|Busy|BuildError|BuildPanic|DoubleRelease|MaxEntries' ./...` and confirm failure (undefined).
- [ ] **Step 3: Implement** `manager.go` (singleflight per key, reference counts, a ticker goroutine started by `NewManager` calling `Sweep` every `IdleTTL/10` (at least 1 s) and stopped by `Close`; panics recovered in the build).
- [ ] **Step 4: Run** the same command; pass; `go vet ./...` and `GOARCH=386 go vet ./...` clean.
- [ ] **Step 5: Commit** `share: volume manager (lazy build, leases, idle TTL)`.

### Task 2: View filesystem and flags

**Files:**
- Create: `tools/tsshare/view.go`, `tools/tsshare/view_test.go`
- Modify: `tools/tsshare/main.go` (flags `-view <dir>`, `-shared <dir>`, `-idle-min N` (default 10), mutual exclusion with `-src`, startup lines, wiring of the Manager and the view root), `tools/tsshare/main_test.go` helpers as needed

**Interfaces:**
- Consumes: `Manager`, `Lease`, `SourceSpec` (Task 1); `guard`, `mountFS`, `shutdown`, read log (existing `main.go`).
- Produces: `func newViewRoot(opt ViewOptions, m *Manager) fs.InodeEmbedder` with `ViewOptions{ZipRoot, Shared, Mount string; ListTTL time.Duration; Logf func(string, ...any)}`; pure helper `func viewName(zipName string, taken map[string]bool) (string, bool)` (maps `X.zip` to `X.vhd`, uniqueness, returns false for names that must not be shown); `func shownEntries(dir string, excl []string, ttl) ([]viewEntry, error)`.

- [ ] **Step 1: Write the failing tests.** Unit (`view_test.go`): `TestViewName` (table: `SD.zip`→`SD.vhd`, `A.ZIP`→`A.vhd`, case twins `a.zip`/`A.ZIP` → `a.vhd`, `A (2).vhd`, `x.zip.zip`→hidden/skip rule: a name still containing `.zip` after mapping is skipped, unicode names, hidden `.x.zip` skipped, `.vhd` source file not shown), `TestShownEntries` (temp tree with dirs, zips, `foo.zip.d` directory, symlink dir, the mount dir and shared dir excluded, a zip inside `shared/` not shown at top, hidden names; listing cache honours TTL). FUSE integration (`view_test.go` or `main_test.go`, skipped if no /dev/fuse): `TestViewMountListsAndReads` (mount the daemon with `-view`/`-shared` on a temp tree with two small zips and a shared dir; `os.ReadDir` of the mount lists dirs and `<name>.vhd`, `shared.vhd`, no `share`, no `.zip`; for each `.vhd`: `fsck.vfat -n` on the dd-extracted partition and `mcopy` byte comparison with the zip/dir contents), `TestViewSizes` (before open `Stat` size equals the real zip size; after `os.Open`, `f.Stat().Size()` equals the built volume size = what a second mount of the same zip with `-src` reports), `TestViewReopenRescan` (add a file to `shared`, close all handles, reopen: new file present; modify a zip: reopen shows new content), `TestViewSwitch` (open zip A, close, open zip B, open A again quickly: no corruption), `TestViewReadOnly` (write/trunc/mkdir/unlink return EROFS or EACCES), `TestViewBusy` (4 distinct sources open at once: the 4th open fails with EBUSY, the others still read), `TestViewNeverRecurses` (mount point inside the zip root appears nowhere, also given as a relative and as a symlinked path), `TestViewBuildFailure` (corrupt zip: open returns EIO, listing and the other entries keep working, daemon alive), `TestViewFlags` (`-view` with `-src` rejected with exit 2; `-shared` missing is created; `-view` without `-mount` rejected).
- [ ] **Step 2: Run** `go test -race -run 'View' ./...` and confirm failure.
- [ ] **Step 3: Implement** `view.go` with go-fuse `fs` nodes (a directory node with `Readdir`/`Lookup`, a file node with `Getattr` (attr timeout 0), `Open` (lease acquire; `FOPEN_DIRECT_IO`; returns EBUSY/EIO on errors), `Read` (via the lease's `Volume.ReadAt`, same guard/log as `vhdFile`), `Release` (lease release), `Setattr`/`Write` EROFS); `main.go` wiring and startup lines: `view: zip root ..., shared ..., mount ...`, `serving view at <mount> (no volumes built yet)`. The daemon mounts immediately (nothing is scanned at start). Existing `-src` mode untouched.
- [ ] **Step 4: Run** `go test -race -count=1 ./...` (the whole package, twice), `go vet ./...`, `GOARCH=386 go vet ./...`, `GOARCH=386 go test ./...`, and the ARM cross-build (`GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`, `file` says static ARM EABI5).
- [ ] **Step 5: Commit** `share: view mode (zip names, shared folder, lazy volumes)`.

### Task 3: Scripts, README, docs

**Files:**
- Modify: `tools/tsshare/scripts/tsshare_start.sh`, `tools/tsshare/scripts/tsshare_stop.sh`, `tools/tsshare/scripts/test_scripts.sh`, `tools/tsshare/README.md`, `docs/TASK-share.md` (status line)

- [ ] **Step 1: Start script view mode.** Settings block gains `ZIPROOT=/media/fat/games/TSConf`, `SHARED=/media/fat/games/TSConf/shared`, `MODE=view` (`MODE=single` keeps the old behaviour with `SRC`); default `MOUNT` in view mode `/media/fat/games/TSConf/share`. In view mode: create `$MOUNT` and `$SHARED` if missing (print one line each), run `tsshare -view "$ZIPROOT" -shared "$SHARED" -mount "$MOUNT" -cache-mb ... -label ...`, wait for the mount (`fuse.tsshare` in /proc/mounts and `$MOUNT/shared.vhd` present), print a short summary and the OSD path (`Mount SD2 image -> share -> <name>.vhd`). All existing safety checks (pid validation by argv0 `tsshare` and `-mount`, exact fstype match, 60-column output, BusyBox-safe constructs, log creatable check) carry over; the single-source mode and its tests keep passing. `tsshare_stop.sh` unchanged except messages.
- [ ] **Step 2: Test script** `test_scripts.sh`: add a view-mode dry run (zip root with one zip and a subdirectory, shared folder, mount in a temp dir): start refuses nothing wrongly, `shared.vhd` and `<name>.vhd` appear, a second start refuses, stop unmounts; plus the pid/stale/foreign-mount tests still pass.
- [ ] **Step 3: README** (view mode section, why `X.vhd` and not `X.zip` with the Main_MiSTer facts and citations, the picker flow, memory/time costs (first open of a big zip pauses the OSD while it is scanned: 17 s for the 57k-entry zip on the board), no core change, limits, flags table updated), fix any text that says `share.vhd` only; update `docs/TASK-share.md` status line.
- [ ] **Step 4: Run** `sh -n`, `bash --posix -n`, `scripts/test_scripts.sh` twice (nothing left behind), `go vet ./...`. **Commit** `share: view mode in start script, README` (scripts and README may be two commits).

### Task 4: Board test (owner present; controller drives the OSD)

- [ ] **Step 1:** ask the owner / check `/tmp/CORENAME` is `MENU`; confirm the zip root and that creating `games/TSConf/share` and `games/TSConf/shared` is approved (it is: owner said so). Put a small zip and a few files into `games/TSConf/shared` only if the owner wants a visible test (offer; otherwise use a tiny test file the controller removes afterwards).
- [ ] **Step 2:** cross-build, deploy binary and scripts (`scp -O`, md5 check), run `tsshare_start.sh` (view mode): check `/proc/mounts`, `ls games/TSConf/share`, idle RSS, listing time of the view root.
- [ ] **Step 3:** OSD (via the Companion Remote daemon, watch the HDMI capture `/dev/video4` feed, never `/dev/video0..3`): load the dual-VHD core, mount image 1 (copy of `vhd-example.vhd` outside `games/TSConf`), Mount SD2 image, browse to `share/`, check the picker shows the zips as `<name>.vhd`, `shared.vhd`, the subfolders and no raw `.zip` entries inside `share/`; pick the 1.4 GB zip (note the OSD pause, expect about 17 s), check WC, play a PT3, then reset and pick another source (`shared.vhd`) and back; check `Not Enough Space` on a write attempt; stop with the image mounted; verify what Main_MiSTer does with a remembered `share/...vhd` path when the daemon is not running (start the core without the daemon).
- [ ] **Step 4:** cleanup (restore `menu.rbf`, remove test files and test dirs the controller created, stop the daemon, leave binary/scripts installed unless the owner says otherwise); write `tools/tsshare/docs/BOARD.md` section for view mode; commit `share: board results for view mode`.
- [ ] **Step 5:** offer merge/push to the owner; do not push without the owner's word.

---

## Self-review

- Spec coverage: view mount point and zip root (T2, T3), name mapping and exclusions (T2), fixed folder with rescan on reopen (T1, T2), no core change (Global Constraints), manager rules from the builder check (T1), scripts and creation of `share`/`shared` (T3), board checks including the remembered-path question (T4).
- Type consistency: `SourceSpec`, `Manager`, `Lease` defined in T1 and used unchanged in T2; `viewName`/`shownEntries` defined in T2.
