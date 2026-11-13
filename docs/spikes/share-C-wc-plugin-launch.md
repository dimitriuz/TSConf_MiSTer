# Spike C: Wild Commander plugin API (tslabs/zx-evo, pentevo/soft/WC/source, clone f0eebed)
Cites: R = plugins/READ_ME.TXT (line n). Code files are CP866 with CR line ends; line numbers are after `tr '\r' '\n'`.
WCVW=WCVW.ASM, MD=MD20.ASM, FX=WCFX.ASM, CORE=core32/CORE32.ASM.
Caveat: READ_ME says API #13; code's PLGCV (MD:23) is #12 and WCVW:493 rejects > PLGCV. The source tree may lag the binary. Unconfirmed.

## 1. Programmatic "open this file" / start another plugin: NO
- API table FUX (MD ~1380-1465) holds funcs 0-86: UI, keys, file I/O (48-63, 72-75), video, params. No "run file", "set current file", "re-dispatch", or "start plugin".
- Exit codes are only 0,1,2,3,4 (R:75-81). Dispatcher: WCVW.ASM EXEcute: `OR A:JP Z,GED / DEC A:JR Z,VIK / DEC A:JR Z,NXTFL / DEC A / DEC A:JR Z,PRVFL`.
- Exit 3: GOPLG does `CP 3:CALL Z,SEXY` (WCVW GOPLG "Pf"). SEXY (FX:645) = TLSTCAT + PANELI + FOLD (re-scan dir), then `XOR A:RET`, so A=0 and the main loop goes to GED. 3 cannot chain into 2/4 or "open file X".
- Keyboard injection: none. ENKE/KBSCN etc. poll the PS/2 driver (PS2P.ASM); no buffer-write function exists. Plugin could only fake it via INT_PL (func 86) own handler, which does not touch WC's own key state: unconfirmed, unlikely to work.
- Closest trick: plugin exits 2 or 4 (see 2); WC moves the panel cursor and dispatches the file under it. Cursor targets cannot be set by name.

## 2. Exit 2 / 4 (next / previous)
- WCVW `NXTFL CALL CRDN:JR nxtFL` / `PRVFL CALL CRUP` / `nxtFL CALL STATUS` then falls into `VIEW`. So: move the cursor +1/-1 in the ACTIVE panel (CRDN/CRUP in MD:316-345), then run the same path as F3.
- Next/prev goes through VIEW, so F3ENTFL=1 ("F3 mode"), not Enter mode (WCVW EXEcute: `LD (F3ENTFL),A`; VIEW sets A=1). GOPLG (WCVW ~782-800) filters by header +63: bit0=1,bit1=0 means Enter-only, so a plugin flagged Enter-only is NOT picked on next/prev. +63 = 0 matches both.
- It re-dispatches by extension of the new entry (GETEN gives ext at ENTRY+8, MD:382-400): matching plugin is called afresh with type 0, BC=name, DE:HL=size. Same plugin only if its ext table matches. No match: `VIK: LD A,(F3ENTFL):OR A:JP NZ,EXTSEL`, i.e. the "choose viewer" menu opens (needs user input).
- Directories are NOT skipped: VIEW does `GETEN:JP Z,GED` (Z = flag byte 0 = dir/".."), so the cursor lands on the dir and WC stops there (chain ends). Files without plugin ext are NOT skipped either (EXTSEL menu).
- End of dir: CRDN/CRUP simply fail to move (MD:316-345) and the same file would be reopened: plugin must check HL'==DE' itself (R:65 "if HL'=DE' ... pointless"). Same at top for 4 (HL'==1, my inference).
- Source is the panel's in-RAM listing (NEWDIR "GENERATING DIRECTORY IN RAM", MD:~488), built once when the dir is entered/re-read, not live FAT. A file created by MKfile is invisible to next/prev until exit 3 (TLSTCAT+NEWDIR). Exit 3 returns A=0, so nothing is chained after the reread. Cursor index (IX+12/16) is kept (PANK clamps), not the filename: with name sort, new entries shift positions.
- Order = panel order, including sort setting (by name/ext/size), mixed dirs/files.

## 3. Custom extensions
- Header +64: 32 slots x 3 bytes (NOT 3 x 32). R:128-129 "3*32" = 3 chars x 32 exts; +61 = count 1-32 (0 = all); +60..: end at first slot starting with 0 (CHEXT in WCVW:~81 stops on `OR A:RET Z`, stride 3 patched by CHext). A' = ext index 0-31 on entry (R:63). Max file size filter at +161.
- Extension is the last 3 chars after the last '.' of the long name, uppercased, space-padded (BDSH in MD:~565-585: `LD A,".":CPDR`, EXTCOPY of 3 chars; GETEN copies to ENTRY+8). So TRACK.PT3.LNK -> "LNK"; `*.SHR` -> "SHR". Only the final suffix, only 3 chars; longer ext (.LINK) would be truncated to first 3 chars after the dot ("LIN"): from code reading, untested. Caps-only (R:129).
- Enter on such file calls plugin with type #00 (+197=#00 means "by extension only", R:162). So placeholders route to our plugin. After exit 2 the next placeholder is routed the same way provided +63 allows F3 mode (see 2). Plugin can also be loaded from INI [PLUGINS] (WCINI SUPL) or by Enter on .WMF (WCVLS:310).

## 4. State between calls: YES, resident
- Plugins are loaded once (LDPLG/LDRE in WCVW:~484-570, LOAD512 into plugin RAM pages PLGPG..) and stay until deleted from F10 menu (PSDEL/PLGDEL). Each call is just `CALL #8000` (R:47) into already-resident pages, not a reload.
- R:96-103: header +34 page count may include pages "used solely as buffer, never touched even after exit, store data for next launch". So persistent data = those pages. Total budget: plugin <=128 KB (R:115), 16 KB/page; pool size PLEPG-PLGPG in MD (WCVW:~497 `CP PLEPG-PLGPG`). No dynamic alloc call: pages are fixed at load time via header.
- Caveat: in-loaded header blocks (+36 table) fill pages from disk at load; buffers beyond that are zero/unspecified at load. Also GEDPL/screens may be changed by caller, so re-map with MNG*_PL (funcs 0,78,79).
- Deleting/reloading the plugin ("Plugin already exists", RELOAD7) drops state.

## 5. Overwrite / truncate
- Only: FENTRY(59), GFILE(62), LOAD512(48), SAVE512(49), MKfile(72), MKdir, REName(74), DelFl(75). No truncate/resize/overwrite-size call.
- MKfile i:HL=flag(1),length(4),name,0 (R:312-322; CORE MKFILE 1322): allocates clusters for the length given, errors if name exists (code 3 long / 4 short, R:361-364). So for placeholder-with-size-0: DelFl then MKfile with real length then SAVE512 (MKfile leaves position at start of file, R:321).
- SAVE512 on an existing file (after FENTRY+GFILE) overwrites data in place within the existing cluster chain but cannot grow it (inference from CORE SAVE512 463: chain walk, no extension; unconfirmed). 0-byte placeholder has no clusters: delete+recreate.
- After MKfile/LOAD512/SAVE512 the FindNext cache is trashed (R:276).

## Verdict for the NeoGS/TSConf idea
Plugin cannot launch handler for a file it just created. Possible pattern: placeholder *.LNK routed to our plugin; plugin DelFl+MKfile+SAVE512 the data, exits 3 (reread) and user presses Enter again. Or exit 2/4 to walk to the next entry (chain only goes through panel listing as of last read).
