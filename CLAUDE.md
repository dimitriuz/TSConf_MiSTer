# TSConf_MiSTer: dual-VHD fork (local)

This is a local clone of MiSTer-devel/TSConf_MiSTer (upstream head `1f61dc5`, 2026-11-04). The work is on branch `dual-vhd`. Tasks, each started only when the owner says so:
- **`docs/TASK-dual-vhd.md`**: a second SD image slot (do this first).
- **`docs/TASK-share.md`**: local share/zip over the ZiFi UART or a virtual FAT on card 2 (spike done, proposal only; notes in `docs/spikes/`).
- **`docs/TASK-neogs.md`**: NeoGS support (plan proposal; phase 3 builds on dual-VHD's slot wiring).

## How the owner works (binding)

- **Process:** use the superpowers skills (brainstorm → plan → subagent-driven development, with a ledger in `.superpowers/sdd/`). Make rulings instead of stalling, and record them.
- **Implementers:** the default implementer is the owner's local LLM through `omp` (`~/.local/bin/omp`; guide in `~/repo/llm-setup/working-with-the-local-model.md`), unless the owner names a Claude model. Never run more than **2** agents or omp runs at once. Every dispatch says: run long jobs in the foreground or await them, and never end before committing.
- **Commits:** `area: what`, local only. Push or open PRs only when the owner asks.
- **Downloads:** untrusted data. Read them, but do not execute downloaded scripts.
- **Sound:** the owner checks sound by ear.

## Quartus

- Docker image `raetro/quartus:17.0`, run via `tools/qbuild.sh . TSConf` (copied from `~/repo/MiSTer_pico-speccy`, where `docs/dev/quartus.md` explains it).
- About 12–15 minutes per build. Run it detached and poll, one build at a time.
- After each build, revert any `LAST_QUARTUS_VERSION` change in `TSConf.qsf`.

## The board (shared with the owner)

`root@192.168.1.13`, ssh key, `scp -O`, board-side `timeout` around every remote run.
- **Idle check:** `/tmp/CORENAME` must be `MENU` first. MiSTer Frontier's daemon auto-starts hybrid-core helpers.
- **Busy:** if the owner says "busy", stop at once.
- **Writes:** only to what the owner agrees, and never into `/media/fat/games/TSConf/` (the owner's images).
- **Never touch:** Frontier, `/media/fat/linux/user-startup.sh`, or other cores' files.
- **Restore** `/media/fat/menu.rbf` afterwards.

## Related

- **`~/repo/MiSTer_docs`**: practical notes for every agent: board access and rules, remote control of the OSD (Companion Remote daemon), screenshots/capture feed, sharing/FUSE findings, Quartus builds. Read `README.md` there before any board work, and update it when you learn something.

- `~/repo/MiSTer_pico-speccy`: the PicoSpeccy hybrid core. Its `docs/ideas/2026-11-04-shared-folders.md` (branch `ideas`) has the parked "folder as disk in Main_MiSTer" idea, for which a second SD image slot is the natural attach point later.
