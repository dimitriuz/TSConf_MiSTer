# Task: a second SD image slot for the TSConf core (dual VHD)

**Status: ready to start when the owner says so.** Do not start on your own.

## Goal

TSConf's second SD card (Z-Controller chip-select 2) can be a **second VHD/IMG file** mounted from the OSD, instead of only the physical secondary SD card. The owner has no secondary SD slot.

**Backwards compatibility:** with no second image mounted, everything behaves exactly as today, including the physical-card fallback for owners who have the real slot.

## Facts from the source (checked 2026-11-04 on upstream `1f61dc5`, "Major TSConf core update (#11)")

- `TSConf.sv:71`: there is a single slot, `"SC0,VHD,Mount SD image;"`.
- `TSConf.sv:207`: `hps_io #(.CONF_STR(CONF_STR)) hps_io`, with no `VDNUM`, so it defaults to 1 (`sys/hps_io.sv:35`, `VDNUM 1..10`). Its ports are per-slot arrays: `sd_lba[VDNUM]`, `sd_blk_cnt[VDNUM]`, `sd_buff_din[VDNUM]`, … (`sys/hps_io.sv:133-142`). TSConf wraps its scalar wires in one-element array literals (`.sd_lba('{sd_lba})`, `.sd_buff_din('{sd_buff_din})`, around `TSConf.sv:226-236`). Read `sys/hps_io.sv` for the exact widths of `sd_rd`, `sd_wr`, `sd_ack`, `img_mounted` and the rest with `VDNUM=2`.
- The machine already has **two SD chip-selects**: `.SD_CS_N(sdss)`, `.SD_CS2_N(sdss2)` (`TSConf.sv:~416-420`, ports of `rtl/tsconf.v`).
- Routing today, at `TSConf.sv:560-599`:
  - `vsd_sel <= |img_size` on `img_mounted`, plus a reset pulse (`reset_img`, 10,000,000 `clk_sys` cycles).
  - MISO mux: `!vsd_sel ? SD_MISO : !sdss ? vsdmiso : !sdss2 ? SD_MISO : 1'b1`.
  - One `sd_card` instance with `.ss(~vsd_sel | sdss)`.
  - Physical pins: `SD_CS = vsd_sel ? sdss2 : sdss`, `SD_SCK/SD_MOSI` gated by `~SD_CS`.

  So with a VHD mounted it is card 1, and the physical card moves to card 2. Without one, the physical card is card 1.
- LEDs: `LED_USER = (vsd_sel & sd_act) | ioctl_download`, `LED_DISK = {1'b1, ~vsd_sel & sd_act}` (`TSConf.sv:476-477`).
- The NVRAM page already offers TS-BIOS boot device **"SD2 Z-controller"** (`TSConf.sv:~98`, `P1oKM`). Booting from card 2 is a BIOS feature, so no core change is needed for it.

## The change

1. **CONF_STR:** add `"SC1,VHD,Mount SD2 image;"` right after the `SC0` line. Keep `SC0`'s text unchanged.
2. **`hps_io`:** `#(.CONF_STR(CONF_STR), .VDNUM(2))`.
   - Widen the `sd_*` and `img_*` wires to two slots. Pass real two-element arrays for `sd_lba` and `sd_buff_din`. `sd_rd`, `sd_wr`, `sd_ack` and `img_mounted` become `[1:0]` vectors.
   - Keep `sd_blk_cnt` at its default, or tie it to 0 the way the current code does if it does.
   - Both `sd_card` instances share `sd_buff_addr`, `sd_buff_dout` and `sd_buff_wr`; each has its own `sd_buff_din[i]`, `sd_lba[i]`, `sd_rd[i]`, `sd_wr[i]` and `sd_ack[i]`. Check `sys/sd_card.sv` for its port list before wiring. The current instance uses `.*`, so replace that with explicit connections for both instances.
3. **Mount state per slot:**
   - `vsd_sel[0] <= |img_size` when `img_mounted[0]`;
   - `vsd_sel[1] <= |img_size` when `img_mounted[1]`;
   - `img_size` is shared and valid together with the `img_mounted` bit.
   - Keep the reset pulse on either mount.
4. **Routing:**

   | state | card 1 (`sdss`) | card 2 (`sdss2`) | physical SD pins |
   |---|---|---|---|
   | no images | physical | — | CS = `sdss` (today) |
   | image 1 only | VHD 1 | physical | CS = `sdss2` (today) |
   | image 2 only | physical | VHD 2 | CS = `sdss` |
   | images 1 + 2 | VHD 1 | VHD 2 | deselected (CS = 1) |

   - Instance 0 is `.ss(~vsd_sel[0] | sdss)` and instance 1 is `.ss(~vsd_sel[1] | sdss2)`.
   - MISO is the selected virtual card's `miso`, otherwise the physical `SD_MISO` when the physical card owns the active chip-select, otherwise `1`.
   - The physical card is selected only by a chip-select not taken by a VHD.
5. **LEDs:** `LED_USER` lights for virtual-card activity on either slot. `LED_DISK` lights for physical-card activity only. Mirror the current intent; `sd_act` needs to be derived per slot. Read where `sd_act` comes from.
6. **README:** add one line under the SD-card text: "Mount SD2 image: a second VHD as card #2; the physical secondary SD card is used only for a card slot with no image mounted."

## Build

- Use Quartus 17.0 Lite in Docker: `tools/qbuild.sh . TSConf`. The script is copied from `~/repo/MiSTer_pico-speccy`; read its header.
  - A build takes about 12–15 minutes. Launch it detached (`setsid nohup tools/qbuild.sh . TSConf </dev/null > out/qbuild.log 2>&1 &`) and poll in the foreground. Never run two builds at once (the host has about 5 GB of free RAM).
  - After each build, revert any `LAST_QUARTUS_VERSION` line Quartus rewrote in `TSConf.qsf`.
- The output is `output_files/TSConf.rbf`. Copy it to `releases/TSConf_dualsd_YYYYMMDD.rbf`, or keep it out of git and deliver it to the board only.
- Check the timing report: no new negative slack on `clk_sys` paths.

## Board test (owner's MiSTer `root@192.168.1.13`; the controller or the owner runs it)

Rules: the board is shared.
- Check that it is idle (`/tmp/CORENAME` = `MENU`), ask the owner before any board work, and stop at once on "busy".
- Write only `/media/fat/_Other/TSConf_dualsd_*.rbf` (agree the folder with the owner first) and `/tmp`.
- Never touch `/media/fat/games/TSConf/*` (the owner's images are read-only input). Never touch Frontier or `user-startup.sh`.
- Restore `menu.rbf` afterwards.

Checks:
1. Image 1 = the owner's `boot.vhd`, no image 2: TS-BIOS boots Wild Commander as before. This is the regression check.
2. Image 1 = `boot.vhd`, image 2 = a second VHD made for the test: WC lists **both** SD cards (Alt+F1/F2), and files from card 2 open.
   - Make the second VHD as a copy of a small FAT32 image in `/tmp` or the agreed folder, for example 256 MiB with `mkfs.vfat -F 32` and an MBR partition at sector 128, plus a few `.trd`/`.spg` files.
   - Remember FAT32 needs at least 65,525 clusters, so use 512-byte or 2 KB clusters at 256 MiB.
3. Set the NVRAM boot device to "SD2 Z-controller" with image 2 holding `boot.$c` and `WC/`: it boots from card 2.
4. Remounting or ejecting either image resets cleanly, and the other slot keeps working.

## Delivery

- Work on branch `dual-vhd`, with commits of the form `area: what`. Push only when the owner asks.
- Optionally offer the change upstream as a PR to MiSTer-devel/TSConf_MiSTer once it is board-verified. That is the owner's decision.
