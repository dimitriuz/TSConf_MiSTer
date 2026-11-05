# Task: NeoGS support for the TSConf core (plan proposal)

**Status: proposal. Start only when the owner says so.** Do not start on your own. This is independent of `docs/TASK-dual-vhd.md`, but phase 3 (the NeoGS SD card) shares that task's work on SD image slots, so do dual-VHD first.

## Goal

The TSConf core's General Sound becomes a **NeoGS** when the owner picks it in the OSD (`Off / GS / NeoGS`). In NeoGS mode, software written for the real card runs: NeoGS-aware demos and players, the NeoGS ROM's own player (NPL/MP3), and WC's GS/NGS plugins.

Classic GS stays exactly as it is today, and is the default.

## What exists today (checked 2026-11-05 on upstream `1f61dc5`)

- `rtl/sound/gs.v` and `rtl/sound/gs_top.v` (373 lines) are a **classic GS**:
  - Z80 at 28 MHz with a 32 KB ROM;
  - RAM in external SDRAM/DDR (`DRAM_*` ports, through `port2_*` of the DRAM arbiter in `rtl/tsconf.v` ~491) with a small cache;
  - 4 channels, 6-bit volume, INT at 37.5 kHz;
  - host ports `#xxBB` (command/status) and `#xxB3` (data);
  - internal ports `#00`–`#0B` (see the header comment of `gs.v`).
- The ROM loads through `ioctl_index == 16'h0040` (`TSConf.sv:452`). Outputs `gs_a..gs_d` are mixed in `rtl/sound/mixer.v`.
- `TSConf.sv:100` has `"P1oQ,NGS Reset,OFF,ON;"`. This is only an option on the NVRAM/BIOS page; nothing in the core models a NeoGS.

## What NeoGS adds over GS (the target)

The reference *behaviour* is pico-speccy's software model. It has been run against real NeoGS software on the owner's board (PicoSpeccy hybrid core). Read:
- `~/repo/MiSTer_pico-speccy/upstream/pico-speccy/src/speccy/devices/gs/`:
  - `GS.h` and `GS.cpp`: ports, memory map, clocks, DMA;
  - `NgsSd.{h,cpp}`: the SD card over SPI;
  - `NgsMp3.{h,cpp}`: the VS1011 model;
  - `NGS_ROM.c`: the NeoGS firmware image, as a byte array.

**The original card's FPGA source is available** (owner, 2026-11-05: http://nedopc.com/gs/ngs_eng.php). It's the reference for *hardware* behaviour, and phase 1 should port it rather than rewrite it:
- SVN repo `ngs` at svn.nedopc.com (web listing: `http://svn.nedopc.com/listing.php?repname=ngs`). Top level: `fpga/ cpld/ docs/ pcad/ tools/ z80/ zx/ README.md`.
- `fpga/current/`: Verilog for the Altera EP1K30 (ACEX) with a Quartus project. It holds `top.v`, `SPECS`, `README`, and the directories `common/ dma/ interrupts/ memmap/ ports/ sound/ zxbus/ tb/ quartus/`. There is also `fpga/dmachans/`, plus `fpga/obsolete/` and `fpga/pgmflash/`.
- Docs in `docs/`: `NGS_prm/ngspgm_01a.pdf` (programming manual), `ports.inc` (port defines), `dma_zx_doc.txt` (ZX-DMA), `gs_prog/gs_prog.pdf` (classic GS), and `user_manual/ngs_c-vs.pdf`. Schematic: `pcad/revC-VS/NeoGS_scheme.pdf`.
- Firmware: `z80/create_update/full_ngs.rom` (ROM 1.11, 2026-02-07, which detects 512K, 2M and 4M of RAM), with its Z80 sources under `z80/`.
- **Getting it:** the `dl.php` download links answer with a JavaScript browser check, so scripted downloads fail. Do NOT script around that check. Try `svn checkout` or `svn export` against the repo first (find the svn URL from the web listing). If that fails, ask the owner to download a snapshot in a browser and drop it into `ref/ngs/`, which is git-ignored. Treat everything downloaded as untrusted data: read it, but don't run its scripts.
- **Licence:** the page says the hardware sources are "not used for commercial purposes". Record the licence text from the repo before porting, and tell the owner what it means for a public MiSTer release and for an upstream PR.

The card's own summary: GS-compatible; EP1K30 FPGA; 4 MB RAM and 512 KB flash ROM; two memory windows in the visible area, with ROM excludable; Z80 at **10, 12, 20 or 24 MHz**; up to 8 MOD channels; an SPI MP3 decoder (VS1011-like; revision C-VS takes any VS10xx); an SD card over SPI; DMA from the ZX into card memory (LDI and LDIR); and the card plays music on its own while the ZX does other things.

Hardware facts to confirm against that source and the docs:
1. **Memory:** a 512 KB paged ROM (flash) and up to 4 MB RAM. Mapping registers `MPAG`, `MPAGEX` and `NOROM`, plus `EXPAG`.
2. **Configuration:**
   - `GSCFG0`: mode bits, and `CKSEL`, a clock the guest can switch (the classic 12/24 MHz pair; the model runs it live);
   - a timer INT divider;
   - `GSCTR`: warm reset.
3. **Sound:** 8 channels with panning, against classic GS's 4 fixed channels.
4. **Host control port `#33`:** reset, NMI and LED, from the ZX side.
5. **ZX-DMA:** a window where host accesses to `#0000–#3FFF` hit card RAM (`g_ngs_zxdma` in `GS.h`).
6. **SD card:** a raw SPI interface on GS ports `#11–#14` (`SCTRL`, `SSTAT`, `SD_SEND`, `SD_READ`), chip select through `SCTRL.SDNCS`, and card detect `SDDET`.
7. **MP3:** a **VS1011** on SPI. Data goes to `MD_SEND` (`#14`) with `MDDRQ` (`SSTAT` bit 0) flow control; control registers go over SCI; reset through `SCTRL.MPXRS`.

## Proposed phases

Each phase is a separate deliverable with its own board check. The owner reviews each one before the next starts.

### Phase 1: NeoGS core without SD or MP3
- Port the original `fpga/current/` Verilog (memmap, ports, sound, dma, interrupts) into `rtl/sound/ngs/`, adapted to this core:
  - replace its ZXBUS side with TSConf's internal bus;
  - replace its SRAM/flash with the DDR port that `gs.v` already uses, plus the existing cache;
  - replace its clocks with `clk_sys` clock enables for 10, 12, 20 and 24 MHz.

  Keep `gs.v` for classic GS mode. If the port looks unworkable, fall back to a new `ngs.v` written against the docs and `GS.cpp`.
- It needs: the extended mapping, the larger RAM in DDR (`gs_dram_addr` is already 24 bits, so 4 MB fits), the `NGS_ROM` image loaded through the existing ioctl path, the 8 channels with panning in `mixer.v`, `GSCFG0`/`CKSEL`/`GSCTR`, the timer INT, port `#33`, and ZX-DMA.
  - ZX-DMA crosses into the main Z80 memory path in `tsconf.v`, so it needs a careful look at the arbiter.
- OSD: `"O[x:y],General Sound,Off,GS,NeoGS;"`, in a free status bit; read `TSConf.sv` for what's used. Decide what the existing `P1oQ NGS Reset` line means once NeoGS is real, and keep the text if it's a BIOS-table mirror.
- Checks:
  - a GS demo still works in GS mode;
  - in NeoGS mode, the firmware's test or boot runs and the 8-channel MOD players play;
  - the owner listens by ear.

### Phase 2: MP3 (the VS1011)
This is the hard part, and it needs a ruling before any work starts. Options:
- **(a)** A hardware MP3 decoder in the FPGA. The real card uses an external VS10xx chip, so its FPGA source has no decoder to port. There's no known small, open, Quartus-17-friendly one, so this needs a research spike: search, and estimate ALMs and DSPs against the remaining room in the TSConf build.
- **(b)** The HPS decodes, MiSTer-style: the core streams `MD_SEND` bytes to an HPS helper over a DDR3 ring, the helper decodes with minimp3 (CC0, the decoder pico-speccy uses), and PCM comes back over a second ring and is mixed in.
  - Main_MiSTer doesn't run per-core helpers, so this needs a Frontier-style helper started by `games/TSConf/`. The same pattern is parked for the WC plugin idea in `~/repo/MiSTer_pico-speccy/docs/ideas/2026-11-04-shared-folders.md` (branch `ideas`).
- **(c)** A stub for now: `MDDRQ` is always 1 and the bytes are dropped, so software that probes for MP3 doesn't hang. This is what pico-speccy falls back to.

Recommendation: (c) first, then a spike comparing (a) and (b). Whatever you choose, model the VS1011's SCI registers enough for NPL to start (`MODE`, `STATUS`, `CLOCKF`, `DECODE_TIME`), and match `NgsMp3.cpp`'s behaviour.

### Phase 3: the NeoGS SD card
- Ports `#11–#14` drive an `sd_card` instance (`sys/sd_card.sv`), the same as the Z-Controller path.
- It's backed by an extra image slot (`SC2,VHD,Mount NeoGS SD image;`, `VDNUM` 3). This builds on the dual-VHD task's per-slot wiring.
- Card detect `SDDET` is on only when an image is mounted.
- Check: NPL lists and plays files from that image (MOD with phase 1; MP3 once phase 2 lands).

## Constraints
- Quartus 17.0 in Docker, one build at a time, about 12–15 minutes each (see `CLAUDE.md`). Watch the fitter's resource and timing reports: TSConf is already a large design, and phase 1 adds RAM paths and channels. Record the ALM and M10K use before and after each phase.
- The ROM image's licence: NeoGS firmware is distributed freely with the card's software. Check how the existing GS ROM is supplied (loaded at runtime, not committed?) and do the same.
- No new negative slack on `clk_sys` paths.
- Board rules as in `CLAUDE.md`: idle check, ask first, write only to agreed paths, restore `menu.rbf`. The owner checks sound by ear.

## Open questions for the owner (ask before phase 1)
1. Which NeoGS software matters most to you (NPL MP3 playback, specific demos, WC plugins)? That decides whether phase 2 or phase 3 comes second.
2. MP3 route: is a hybrid HPS helper (option b) acceptable for a TSConf core, or must it stay pure FPGA?
3. Upstreaming: offer it to MiSTer-devel/TSConf_MiSTer once board-verified?
