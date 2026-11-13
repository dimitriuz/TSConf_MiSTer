# Spike A: TSConf UART / ZiFi RTL (read-only)

## 1. TSConf.sv wiring
- CONF_STR "UART115200,MIDI;" TSConf.sv:70. Declares to Main_MiSTer a UART at 115200 with a "MIDI" mode option.
  hps_io decodes 0x3B into {uart_speed, uart_mode} (sys/hps_io.sv:535-540); uart_mode wire TSConf.sv:182, hps_io port :217.
  uart_speed is NOT connected/used by the core (grep: no consumer in TSConf.sv) -> the baud is fixed in RTL.
- UART_RTS=0, UART_DTR=0 constants (TSConf.sv:30-31). UART_CTS/DSR inputs unused. No hardware flow control at all.
- UART_RXD goes to emu .UART_RX (:431) and also to .TAPE_IN (:429; shared with tape input).
- UART_TXD = uart_mode==3 ? midi_out : (mode==1||mode==2) ? uart_out : 1 (idle high) (:466-467).
  So MIDI (mode 3, assumed = the "MIDI" option; unsure of exact mode numbering, Main_MiSTer decides) drives TX from AY IO port bit
  (rtl/tsconf.v:1165,1169: MIDI_OUT <= ioa_x_out[2]). Modes 1/2 drive TX from the ZiFi UART. Mode 0 = TX idle.
  RX into ZiFi is not gated by mode (rtl/tsconf.v:1110 rx(UART_RX)).

## 2. UART implementation (rtl/periph/zifi.v, uart_rx.v, uart_tx.v)
- NOT ZX-Uno. It is the ZiFi / HackerVBI "eRS232" API at ports 0x??EF (zifi.v:19-74), instantiated rtl/tsconf.v:1099-1112 on fclk.
  - DR  0x00EF..0xBFEF (casez 16'h??EF): R = pop RX FIFO, W = push TX FIFO (zifi.v:112-116, 143-146).
  - 0xC0EF R ZF_IFR: RX bytes used, capped 191 (reads 0 while rx_busy) :88-92, selects ZIFI FIFO (zifi_en=1).
  - 0xC1EF R ZF_OFR: TX free, capped 191 :93-97.
  - 0xC2EF/0xC3EF RS_IFR/RS_OFR: RS232 FIFO -> stubs: IFR=0, OFR=191, zifi_en=0 (DR then does nothing) :98-107.
  - 0xC7EF W CR: 000000oi clear FIFOs (bit0 RX, bit1 TX), 000001oi no-op, 11110mmm accepted no-op (API mode ignored),
    11111111 get version -> ER=1, else ER=0xFF :122-141. 0xC7EF R ER :108-111.
- FIFOs (Altera scfifo, showahead): RX 8192 x 8 (:171-190), TX 256 x 8 (:204-224). Overflow_checking ON = overflow bytes silently dropped.
- Possible bug (unsure whether intentional): `fifo_tx_freew = 8'h255 - usedw` (:199) -- 8'h255 is hex, truncates to 0x55 (85), so
  reported free = (85 - usedw) mod 256, wrong once TX FIFO holds >85 bytes (the Z80 sees garbage, not 0). Cap 191 compare is 8-bit.
  Only matters for bulk TX; software that keeps small bursts is fine.
- Z80 interrupts: none. No IRQ line from zifi (module has no irq port). Polling only.
- Flow control: none (RTS/DTR tied 0, CTS ignored). No overrun/framing/error flags: uart_rx drops stop-bit check result
  (stop bit just waited, no framing error output), RX FIFO full -> bytes lost silently. ER register is command-result only.
- Workaround "rx_busy" (:153-162): IFR reads 0 for ~1M fclk cycles (20-bit counter, ~37 ms @28 MHz) after each received byte while <191 in FIFO,
  to fix zifi.spg's 191-byte INIR hang. This throttles polling-style readers (a burst at high baud keeps rx_busy asserted) -- relevant to a fast link.
- Format fixed 8N1 (uart_rx.v:5-7, nandland modules). Counters are 8-bit (uart_rx.v:32, uart_tx.v:32) so CLKS_PER_BIT must be <=255.

## 3. Baud and clock
- Both uart_rx and uart_tx hard-coded CLKS_PER_BIT = 28_000_000/115200 = 243 (zifi.v:227,241). Actual baud 28e6/243 = 115226 (+0.02%).
- Clock = fclk: clk_sys 84 MHz (rtl/pll/pll_0002.v:25, ref 50 MHz) divided by 3 via ce_28m + cyclonev_clkena (TSConf.sv:155-172) = 28 MHz.
  (The gated 28 MHz clock is a clock-enable-gated clock, fine for fabric logic.) The same 115200 in MIDI mode? MIDI path is bit-banged from
  AY port, so its baud is whatever Z80 software does (31250 normally).
- The `uart_speed` that Main_MiSTer sends is ignored; no runtime baud change by Z80 (no divisor register).
- Max baud with 28 MHz and this RTL: needs >= ~8 clk/bit for reliable mid-bit sampling => ~3.5 Mbaud nominal; 8-bit counter/integer divisor
  gives: 921600 -> 30.4 (30, -1.3% error), 1 Mbaud -> 28 exact, 2 Mbaud -> 14 exact, 3 Mbaud -> 9.33 (9, 3.7% error, marginal), 3.5M -> 8 exact.
  Realistic: 1-2 Mbaud via divisors 28 / 14 (exact). Using the 84 MHz clk_sys instead would give 3 Mbaud exact (div 28) but is a different clock domain.
- RX sampling: 2-FF synchronizer, start-bit mid-sample, then 1 sample per bit at center; fine for exact divisors.

## 4. Docs
- README.md:61 only: "* [ZiFi](https://github.com/UzixLS/ZiFi) support" and :60 "MIDI output via AY I/O ports". No usage instructions, no
  mention of ESP/AT, baud, or the UART115200,MIDI menu option beyond that. The OSD item itself is the only user doc. zifi.v:20 cites
  HackerVBI/ZiFi eRS232 README. docs/ (TASK files) mention uart only incidentally (TASK-neogs/dual-vhd; not about ZiFi).

## 5. Verdict
- Z80 side is a ZiFi (ports 0xC0EF.. 0xEF data) device, so existing ZiFi software (zifi.spg, UzixLS/HackerVBI tools) works; a Linux daemon
  on /dev/ttyS1 (the HPS UART) can answer AT commands as an ESP8266 emulation at 115200 today with no FPGA change.
- To go faster (921600 / 1-2 Mbaud) the minimal change: change the two CLKS_PER_BIT parameters (zifi.v:227,241), ideally make them a
  localparam shared or a Z80-writable divisor register (e.g. extend CR/ER, 11110mmm is already a no-op slot). Note Main_MiSTer must
  also set the HPS tty to the same baud (core ignores uart_speed, so a daemon would set termios itself). 1 Mbaud (div 28) / 2 Mbaud (div 14) are exact.
- Real issues at speed: (a) no RTS/CTS -> RX FIFO 8 KB drops bytes if Z80 slow (Z80 reading ~ 3.5 MHz port IN loop ~ 100-200 KB/s max
  so >1 Mbaud sustained is unreadable by Z80; 8 KB buffer absorbs bursts only; unsure of exact Z80 loop rate); (b) rx_busy 37 ms throttle makes
  IFR read 0 after each byte -> bad throughput for polling; (c) TX free count bug (:199); (d) no IRQ. HPS side: the DE10 HPS UART can do
  ~3 Mbaud but the FPGA-HPS UART lines need correct kernel settings (unsure).
- Risk: low for a divisor change (2-line RTL, one 12-15 min build), medium for adding a runtime-programmable divisor, and the rx_busy/freew
  fixes carry regression risk for existing zifi.spg (the rx_busy hack exists for it). Software flow control (e.g. the daemon pacing, or
  a ZiFi-API-level protocol) is the practical answer instead of RTS/CTS since the core ties RTS low and ignores CTS.
