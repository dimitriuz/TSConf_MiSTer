# Task: local share / zip as a source for TSConf over the ZiFi UART

**Status: spike done (2026-11-13), proposal only. Do not start on your own.**

**Verdict after the WC source check (spike C):** a ZiFi plugin cannot make a share behave like a local folder for third-party player plugins. Use it to *sync* a folder into a cache directory on card 1, then browse and play normally. For a share that acts as a real drive (open folder, play, auto-next), the virtual FAT volume on card 2 (Main_MiSTer change) is the only route that works without per-track gaps or user keypresses.

## Goal
Browse and fetch files from a folder or zip on the MiSTer's Linux side from a Wild Commander (WC) plugin, over the core's existing ZiFi UART, and open the result transparently. No new SD path.

## What the spike found
Full notes were scratch files; the facts that matter:

- **The core already has ZiFi.** `rtl/periph/zifi.v` implements the HackerVBI/ZX-Evo ZiFi FIFO ports (`#BFEF` data, `#C7EF` cmd/err, `#C0EF` RX used, `#C1EF` TX free; RS232 variant `#C2EF/#C3EF`). RX FIFO 8192 B, TX FIFO 256 B, polling only (no Z80 interrupts), no flow control (`UART_RTS/DTR` tied low, CTS/DSR ignored), fixed 115200 baud (`CLKS_PER_BIT = 28_000_000/115200 = 243`, `zifi.v:227,241`).
- **UzixLS/ZiFi is a Z80 client program, not hardware.** It already downloads files over HTTP on TSConf (zxart, vtrdos, ...). Its README documents the MiSTer setup: OSD *UART Connection = Modem, Link = TCP, 115200*; ZiFi then speaks Hayes (`ATTEL0`, `ATTRANS0`, `ATQ0`, `ATDT<host>:80`) followed by raw HTTP/1.1. HTTP only, no TLS.
- **MiSTer already bridges the UART.** Main_MiSTer `SetUARTMode()` runs `/media/fat/linux/uartmode <mode>` on every mode change; Modem mode (4) starts `midilink` (MidiLink_MiSTer), a Hayes-over-TCP emulator on `/dev/ttyS1`. Its OSD baud list tops out at 115200. No ESP8266-AT emulator exists in MiSTer.
- **Wild Commander plugins** are Z80 assembly `.WMF` files (API v13, `pentevo/soft/WC/source/plugins/READ_ME.TXT` in tslabs/zx-evo). A plugin can create files in the current WC directory (`MKfile` + `SAVE512`) and ask WC to reread the directory (exit code 3). Opening a `.trd` is done by WC through the mounter plugin by extension. Whether a plugin can launch another plugin or file programmatically is **unconfirmed**. No networking WC plugin exists. A plugin must talk to the ZiFi ports itself.
- **WC source check (spike C, tslabs/zx-evo `pentevo/soft/WC`):**
  - **No hand-off.** A plugin cannot start another plugin or open a file by name: the 87-entry function table has no such call, exit codes stop at 4, and exit 3 (reread directory) returns A=0 and ends any next-file chain. No key injection either (WC reads the PS/2 driver directly).
  - **Next/prev (exit 2/4).** WC moves the active panel's cursor one entry and re-dispatches by the new file's extension. Directories stop the chain; a file with no matching plugin opens the viewer menu; at the last entry WC reopens the same file (the plugin must check HL'=DE'). The panel is a RAM listing built at directory read, so a file created by MKfile is invisible until an exit-3 reread.
  - **Extensions.** 32 slots of 3 characters; the extension is the last 3 characters after the final dot, capitalised (`TRACK.PT3.LNK` routes as `LNK`).
  - **State and writes.** Plugins stay resident, and header pages that are never touched keep data between runs (page count fixed in the header). No truncate/overwrite: use DelFl, then MKfile with the real size, then SAVE512.
  - **Unconfirmed.** READ_ME says API #13 but the code accepts up to #12; whether SAVE512 can grow an existing file; exit 4 at the top of the list.
- **Reported, to verify:** `zifi.v:199` computes TX free as `8'h255 - usedw` (that is 85, not 255, so wrong once more than 85 bytes are queued), and an `rx_busy` workaround makes the RX count read 0 for a while after each received byte (`zifi.v:153-162`). Both would hurt throughput.

## Proposed stages
0. **No code, 1 hour.** Run a static HTTP server on the board over a test folder (BusyBox `httpd` if present, else any tiny server), set TSConf's OSD UART to Modem/TCP/115200 and use the existing ZiFi client with `ATDT127.0.0.1:80`. Proves the path end-to-end and gives a speed number. Needs no change to `uartmode` or any other core's files.
1. **WC plugin** `SHARE.WMF`: *folder sync*. List the server's index, download the whole folder (or a chosen subset) with a progress bar into a cache directory on card 1 (DelFl + MKfile + SAVE512), return exit code 3. The user then opens the cache folder and plays or browses as with any local folder, auto-next included. Per-track on-demand download does not work: a placeholder routed to our plugin cannot hand off to the player (no launch call), and a placeholder with the player's own extension would just feed the player an empty file. Zip support is server-side: a tiny daemon unzips on demand, so the Z80 never sees zip. Sync time is folder size over link speed (module/`.pt3` folders are a few MB: about 5 minutes at 115200, well under a minute at 1 Mbaud).
1b. **Alternative to stage 1: virtual FAT on card 2.** Main_MiSTer presents the share as a read-only FAT volume in the second VHD slot built by the dual-VHD work. WC sees a normal drive, so folders, players and auto-next work with no sync and no Z80 code. Cost: a C++ change in Main_MiSTer (virtual FAT builder, lazy file reads, a way to pick a folder for the slot); the folder must not change while mounted. Choose 1 or 1b by whether the sync wait is acceptable.
2. **Speed.** Make `CLKS_PER_BIT` selectable (1 Mbaud = 28 clocks, 2 Mbaud = 14, both exact; 921600 is 1.3 % off). Fix the two ZiFi bugs above without breaking `zifi.spg`. Replace MidiLink's Modem mode with a small purpose-built daemon on `/dev/ttyS1` (MidiLink's list caps at 115200, and no flow control means the daemon must pace itself). Estimate: 115200 gives about 11 KB/s, so a 640 KB `.trd` takes about a minute; 1 Mbaud is about 10x faster if the Z80 poll loop keeps up.
3. **Optional:** upload (write back to the share), directory navigation, cache.

## Open questions for the owner
- Is HTTP over loopback acceptable as the protocol (yes for stage 0), or do you want a custom binary protocol for the plugin?
- Do you accept editing `/media/fat/linux/uartmode` on the board in stage 2? CLAUDE.md says not to touch other cores' files. Stage 0 and 1 avoid it.
- Fork-only (this repo) or also an upstream PR for any `zifi.v` change?

## Not decided
No FPGA change, plugin ABI, or daemon language is chosen. Stage 0's result decides whether stage 2 is worth it.
