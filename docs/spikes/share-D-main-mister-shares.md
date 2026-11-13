# Spike D: Main_MiSTer shared folders (commit 8329c5a, shallow clone in spikeD/src)
Cite base: https://raw.githubusercontent.com/MiSTer-devel/Main_MiSTer/8329c5a2b2721b3a87b145c011b305a6e8115eb4/<file>#L<n>

## 1. Minimig
- No virtual FAT, no block device. It is a core-specific filesystem RPC. support/minimig/minimig_share.cpp (930 lines) implements AmigaDOS packets (ACTION_LOCATE_OBJECT, EXAMINE_NEXT, FINDINPUT, READ, WRITE, SEEK... from miminig_fs_messages.h) on the ARM, backed by a Linux dir.
  An Amiga-side handler ("MiSTerFileSystem", device SHARE:, volume MiSTer; minimig_share.cpp L18-L28) is the driver. It lives on the Amiga side, not in this repo (unconfirmed where shipped; the code comment says it must match MountList).
- Transport = DDR RAM mailbox, NO UIO opcode: shmem_map(0x27FF4000, 0x2000) via /dev/mem (shmem.cpp). Layout: +0 request flag (4B), +4 request buffer (~512B), +0x1000 4KB data buffer (L18-L25).
- Handshake (minimig_share_poll, L899-L921): ARM polls req_id; valid if hi16 == 0x5AA5 and ((req_id-77)&0xFF)==((req_id>>8)&0xFF); processes, then writes the low16 of req_id back into flag+2 as ack. Data transfers: FileReadAdv/FileWriteAdv straight into shmem+DATA_BUFFER (L625, L648).
- Root dir: cfg.shared_folder (ini key shared_folder, cfg.cpp L90) absolute, or relative to HomeDir(); default HomeDir()/"shared" (L262-L281), created with FileCreatePath. HomeDir() = per-core games dir (unconfirmed exact value).
- Hooks: polled from user_io_poll inside `if (is_minimig())` (user_io.cpp L3391 ... call at L3427); minimig_share_reset() from minimig_config.cpp L690. Only the 'is_minimig' gate and the polling call tie it to the core.

## 2. ao486 / x86
- Same pattern: support/x86/x86_share.cpp (1202 lines), DOS redirector (INT2F/"AL_" subfunctions, enum L51-L70: OPEN, READ, WRITE, FINDFIRST, FINDNEXT, GETATTR, ...). A DOS-side TSR/driver is required (unconfirmed which file; not in this repo).
- Mailbox: shmem_map(0x300CE000, 0x2000) (L46-L48). Flag at +0, request at +4, header 8B, data at REQUEST_BUFFER+66 (L50-L53). Magic differs: hi16 == 0xA55A and ((req_id+77)&0xFF)==((req_id>>8)&0xFF), ack = low16 echoed at +2 (x86_share_poll, L1170-L1194). AL_READ reads straight into the shared buffer, len <= 16-bit (L727-L757).
- Hooks: x86_poll() calls x86_share_poll() unless only_ide (x86.cpp L852); reset at x86.cpp L767. Pure polling of DDR; the core uses no UIO opcode for sharing (the FPGA/CPU side just writes DDR; the ao486 core side is not in this repo, unconfirmed).
- Same base-path logic as minimig (L427-L448).

## 3. Generic hooks (no core-specific C++)
- UIO opcode list: user_io.h L12-L106. All ARM-initiated (core is a slave on the SPI/HPS_BUS); the core cannot raise a request except via status reads polled by the ARM (UIO_SECTOR_RD/WR via UIO_GET_SDSTAT sd_rd/sd_wr, UIO_CHK_UPLOAD, UIO_GET_UART flag etc.). No generic "file I/O" or "run command" opcode.
- Generic block path = SD emulation: CONF_STR "SC0,VHD" -> user_io_file_mount (user_io.cpp L2240). It opens ONE regular file with FileOpenEx (L2279). No directory mounting, no virtual FAT, no zip mounting. Zip only appears in file_io.cpp (miniz, L51-L161) for loading ROM-type files via the file selector, not for drives. Special converters exist only per-extension (x2trd for TR-DOS images, L2254; c64 .t64/.d64; Apple II). x2trd (DiskImage.h L7-L8) is a file->TRD image conversion, not folder->image (unconfirmed it handles dirs; it does not by name).
- /dev/MiSTer_cmd FIFO (input.cpp L3692, handler ~L6185-L6230): only fb_cmd, video_mode, load_core, screenshot, volume. Not core-triggerable, external->ARM only.
- /tmp status files: CORENAME, RBFNAME, UART_SPEED, GAMEID, STARTPATH, OSD_VISIBLE (user_io.cpp L1271-L1276, L2749, L1595, L4494). ARM->world, not world->core. STARTPATH/OSD_VISIBLE only when log_file_entry=1.
- ioctl_download (user_io_file_tx, L2874): ARM->core uploads of a file chosen in the OSD; also bootcore/load via file selector. Cannot be started by the core.

## 4. Running ARM code triggered by the core (no Main_MiSTer edits)
- UART path (best): CONF_STR "UART115200,MIDI;" (TSConf.sv L70 already has it) makes Main_MiSTer call SetUARTMode (user_io.cpp ~L1260-L1280): it writes /tmp/CORENAME and /tmp/UART_SPEED, then system("uartmode N") (L1279). `uartmode` is a script on the Linux image (not in this repo; contents unconfirmed here, from memory it bridges the FPGA UART /dev/ttyS1 to console/MIDI/mlinkutil) and N=1/2 = console/UART. TSConf's UART_TXD/RXD already route (TSConf.sv L466). A user daemon on the ARM reading /dev/ttyS1 (started from /media/fat/linux/user-startup.sh or a Scripts menu entry) gets arbitrary bytes from the core => a full request/response channel at 115200 baud (~11 KB/s) or higher (uart speeds menu). Limits: low bandwidth, shared with whatever uartmode started (must stop its getty/bridge: unconfirmed), needs a Z80-side driver/BIOS code.
- user-startup.sh: run at boot only; can start a daemon (inotify, /dev/mem mmap, polling). Limits: no per-core trigger; daemon must poll /tmp/CORENAME itself; survives core changes.
- Scripts menu (menu.cpp L515, L8379): popen of a chosen .sh, user-triggered via OSD only, not by the core.
- /dev/mem from a user daemon: possible (same as shmem_map), root on MiSTer. Needs the core to expose a DDR window (DDRAM_* ports in emu; TSConf does not use them today, unconfirmed). Then the daemon replicates minimig_share_poll itself: no Main_MiSTer change. Risk: Main_MiSTer owns /dev/mem regions per core; 0x27FF4000/0x300CE000 are core-specific, pick an unused one (needs verification vs Main's framebuffer 0x20000000.. and other users).
- inotify on /tmp: only reacts to files Main writes (CORENAME, STARTPATH): useful to start/stop daemon with the core, not to carry requests.

## 5. Verdict for TSConf (ranked by effort)
1. Easiest, no Main change: UART protocol + ARM daemon (user-startup.sh) serving folder/zip as simple file RPC (or exporting a dir as FAT image into the VHD/second slot file on request, then remount via OSD). Needs Z80-side client. Work: daemon ~300-600 lines C/Python + Z80 code.
2. Medium: core exports a DDR window (hps ddram ports) as mailbox, daemon mmaps /dev/mem like minimig/x86. Faster (MB/s), needs core RTL for DDR access + Z80 port logic + daemon. Still no Main edit, but poll runs at daemon's rate and conflicts with Main's use of DDR are unconfirmed.
3. Off-line: Scripts menu entry that builds a FAT/TRD image from a folder/zip into a file, which is then mounted through the normal "SC1,VHD" slot (second VHD). Zero core change, but no live sync; ~100-line script (mtools/mkfs.fat + mcopy). This is the "folder to VHD" workflow.
4. Clean: edit Main_MiSTer. Add support/tsconf/tsconf_share.cpp following minimig_share/x86_share: ~900-1200 lines C++ for a rich RPC (minimig 930, x86 1202), or ~250-400 lines for a minimal read-only protocol (open/read/findfirst/findnext/close); plus ~6 lines in user_io.cpp (is_tsconf gate + poll call + reset), a SHMEM_ADDR choice, and shared_folder reuse. Alternative with less C++: a virtual-FAT/dir-to-image layer inside user_io_file_mount (new file, ~500-800 lines, FAT32 synthesis with writes is the hard part). All needs upstream acceptance or a fork.
Unconfirmed overall: contents of the Linux `uartmode` script; which Z80/Amiga/DOS drivers exist and where; exact HomeDir() value.
