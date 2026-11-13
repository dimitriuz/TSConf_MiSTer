#!/bin/sh
# tsshare_start.sh - start the tsshare daemon (MiSTer Scripts menu).
#
# MODE=view (default): $MOUNT shows every zip under $ZIPROOT as
# <name>.vhd, plus shared.vhd (the folder $SHARED), read-only FAT32 disks.
# Then, in the TSConf OSD: Mount SD2 image -> share -> <name>.vhd.
# MODE=single: one folder or zip as share.vhd in $MOUNT.
# Usage: tsshare_start.sh [zip-root-folder]    (single: [folder-or-zip])
# The argument replaces ZIPROOT (view) or SRC (single). SHARED defaults to
# $ZIPROOT/shared, after that replacement. Paths must be absolute.
# Touches only $MOUNT and $SHARED (view mode creates them) and the
# files in $CONF_DIR.

# ---- settings (edit here, or override with environment variables) ----
MODE="${MODE:-view}"
ZIPROOT="${ZIPROOT:-/media/fat/games/TSConf}"
# SHARED: default $ZIPROOT/shared (set below, once ZIPROOT is final)
SRC="${SRC:-/media/fat/games/Spectrum/compilations/AAA/SD.zip}"
# MOUNT defaults: view /media/fat/games/TSConf/share, single /media/fat/tsshare
CONF_DIR="${CONF_DIR:-/media/fat/Scripts/.config/tsshare}"
BIN="${BIN:-$CONF_DIR/tsshare}"
CACHE_MB="${CACHE_MB:-32}"
LABEL="${LABEL:-TSSHARE}"
# ---- test hooks (used by test_scripts.sh; leave alone on the board) ----
LOG="${LOG:-$CONF_DIR/tsshare.log}"
PIDFILE="${PIDFILE:-$CONF_DIR/tsshare.pid}"
WAIT_SECS="${WAIT_SECS:-90}"
# MOUNTS_FILE: see the lib below
# BIN must be named exactly "tsshare": the scripts recognise the daemon
# by that name. MOUNT: use the real path (symlinked parents are not resolved).
# -----------------------------------------------------------------------

# ---- BEGIN defaults (identical in start and stop; test_scripts.sh diffs it) ----
case "$MODE" in
view | single) ;;
*)
	echo "ERROR: MODE must be view or single"
	exit 1
	;;
esac
ALT=
if [ -z "$MOUNT" ]; then
	if [ "$MODE" = view ]; then
		MOUNT=/media/fat/games/TSConf/share
		ALT=/media/fat/tsshare
	else
		MOUNT=/media/fat/tsshare
		ALT=/media/fat/games/TSConf/share
	fi
fi
# ---- END defaults ----

# normalise MOUNT: no trailing slashes; refuse "" and "/". A symlinked
# parent directory is not resolved (/proc/mounts shows the real path):
# give the real path.
while [ "${MOUNT%/}" != "$MOUNT" ] && [ -n "${MOUNT%/}" ]; do
	MOUNT="${MOUNT%/}"
done
case "$MOUNT" in
"" | /)
	echo "ERROR: MOUNT must be a directory, not / or empty"
	exit 1
	;;
esac

if [ -n "$1" ]; then
	if [ "$MODE" = view ]; then ZIPROOT="$1"; else SRC="$1"; fi
fi

die() {
	echo "ERROR: $1"
	[ -n "$2" ] && echo "  $(short "$2")"
	exit 1
}

# $1 without trailing slashes ("/" and empty stay as they are)
norm() {
	_n="$1"
	while [ "${_n%/}" != "$_n" ] && [ -n "${_n%/}" ]; do
		_n="${_n%/}"
	done
	echo "$_n"
}

need_abs() { # need_abs NAME VALUE
	case "$2" in
	/*) ;;
	*) die "$1 must be an absolute path:" "$2" ;;
	esac
}

# ---- BEGIN lib (identical in start and stop; test_scripts.sh diffs it) ----
# MOUNTS_FILE is a test hook (a sample /proc/mounts); SCAN_PREFIX is
# another (scan_any only looks at processes whose argv[0] starts with it).

# a path for a message of at most 60 columns: the last 52 characters
short() {
	printf '%s\n' "$1" | sed 's/.*\(.\{52\}\)$/...\1/'
}

# $1 as it appears in /proc/mounts (octal escapes for \, space, tab)
esc() {
	printf '%s' "$1" | sed -e 's/\\/\\134/g' -e 's/ /\\040/g' \
		-e "s/$(printf '\t')/\\\\011/g"
}

# fstype of the topmost mount exactly at $MOUNT (empty if none)
mount_type() {
	_m="$(esc "$MOUNT")"
	_t=
	while read -r _d _mp _ty _r; do
		[ "$_mp" = "$_m" ] && _t="$_ty"
	done < "${MOUNTS_FILE:-/proc/mounts}"
	echo "$_t"
}

# only a mount made by tsshare (go-fuse Name "tsshare": fuse.tsshare)
is_mounted() {
	[ "$(mount_type)" = fuse.tsshare ]
}

# lazy unmount: root uses umount -l (the board has no fusermount),
# a normal user (host tests) uses fusermount3 -uz.
# Call only after is_mounted succeeded.
lazy_umount() {
	if [ "$(id -u)" = 0 ]; then
		umount -l "$1" 2>/dev/null && return 0
	fi
	if command -v fusermount3 >/dev/null 2>&1; then
		fusermount3 -uz "$1" 2>/dev/null && return 0
	fi
	umount -l "$1" 2>/dev/null
}

# alive and not a zombie (a dead child of this shell stays a zombie)
alive() {
	kill -0 "$1" 2>/dev/null && \
		! grep -q '^State:.*Z' "/proc/$1/status" 2>/dev/null
}

# is $1 our daemon for $MOUNT? argv[0] basename "tsshare", an argument
# "-mount" followed by $MOUNT, and not this shell or its parent.
is_ours() {
	_p="$1"
	case "$_p" in
	"" | *[!0-9]*) return 1 ;;
	esac
	[ "$_p" != "$$" ] && [ "$_p" != "$PPID" ] || return 1
	alive "$_p" || return 1
	tr '\0' '\n' 2>/dev/null < "/proc/$_p/cmdline" | {
		IFS= read -r _a0 || exit 1
		[ "${_a0##*/}" = tsshare ] || exit 1
		_prev=
		while IFS= read -r _a; do
			[ "$_prev" = -mount ] && [ "$_a" = "$MOUNT" ] && exit 0
			_prev="$_a"
		done
		exit 1
	}
}

# first pid in ps output that is our daemon (BusyBox or procps ps)
scan_pid() {
	for _c in $(ps w 2>/dev/null | grep tsshare | grep -v grep |
		while read -r _x _r; do echo "$_x"; done); do
		if is_ours "$_c"; then
			echo "$_c"
			return 0
		fi
	done
	return 1
}
# the "-mount" value of a live process whose argv[0] basename is
# tsshare (not this shell or its parent); empty if there is none
ts_mount() {
	_p="$1"
	case "$_p" in
	"" | *[!0-9]*) return 1 ;;
	esac
	[ "$_p" != "$$" ] && [ "$_p" != "$PPID" ] || return 1
	alive "$_p" || return 1
	tr '\0' '\n' 2>/dev/null < "/proc/$_p/cmdline" | {
		IFS= read -r _a0 || exit 1
		[ "${_a0##*/}" = tsshare ] || exit 1
		_prev=
		while IFS= read -r _a; do
			if [ "$_prev" = -mount ] && [ -n "$_a" ]; then
				echo "$_a"
				exit 0
			fi
			_prev="$_a"
		done
		exit 1
	}
}

# first process that is a tsshare daemon whatever its mount point
# (scans /proc: a plain ps only lists some processes on some systems)
scan_any() {
	for _c in $(grep -l tsshare /proc/[0-9]*/cmdline 2>/dev/null |
		sed 's|^/proc/\([0-9]*\)/cmdline$|\1|'); do
		_a0="$(tr '\0' '\n' 2>/dev/null < "/proc/$_c/cmdline" | head -n 1)"
		case "$_a0" in
		"$SCAN_PREFIX"*) ;;
		*) continue ;;
		esac
		if [ -n "$(ts_mount "$_c")" ]; then
			echo "$_c"
			return 0
		fi
	done
	return 1
}
# ---- END lib ----

# serving: in view mode also require our own mount, not just the file
ready() {
	[ -e "$MOUNT/$READY" ] || return 1
	[ "$MODE" = view ] && { is_mounted || return 1; }
	return 0
}

# the last log lines without the prefix; a line over 60 columns is shown
# as its first 60 columns, then columns 61-120, or, if it is longer, the
# last 57 columns after "...", so the end of a long message is visible
show_log() {
	[ -f "$LOG" ] || return 0
	tail -n 3 "$LOG" |
		sed 's/^tsshare: \([0-9][0-9]:[0-9:.]* \)\{0,1\}//' |
		while IFS= read -r _l; do
			printf '%s\n' "$_l" | cut -c1-60
			if [ "${#_l}" -gt 120 ]; then
				printf '%s\n' "$_l" | sed 's/.*\(.\{57\}\)$/...\1/'
			elif [ "${#_l}" -gt 60 ]; then
				printf '%s\n' "$_l" | cut -c61-120
			fi
		done
}

[ "${BIN##*/}" = tsshare ] || die "BIN must be named tsshare:" "$BIN"
[ -f "$BIN" ] || die "no daemon at" "$BIN"
[ -x "$BIN" ] || die "not executable (chmod +x):" "$BIN"
need_abs MOUNT "$MOUNT"
if [ "$MODE" = view ]; then
	ZIPROOT="$(norm "$ZIPROOT")"
	SHARED="$(norm "${SHARED:-$ZIPROOT/shared}")"
	need_abs ZIPROOT "$ZIPROOT"
	need_abs SHARED "$SHARED"
	[ "$ZIPROOT" != / ] || die "ZIPROOT must not be /"
	[ "$SHARED" != / ] || die "SHARED must not be /"
	[ -d "$ZIPROOT" ] || die "folder not found:" "$ZIPROOT"
	# textual checks, before anything is created; the daemon resolves
	# symlinks and has the last word
	case "$MOUNT/" in
	"$SHARED"/*) die "MOUNT must not be inside SHARED:" "$MOUNT" ;;
	esac
	case "$SHARED/" in
	"$MOUNT"/*) die "SHARED must not be inside MOUNT:" "$SHARED" ;;
	esac
	case "$ZIPROOT/" in
	"$MOUNT"/*) die "ZIPROOT must not be inside MOUNT:" "$ZIPROOT" ;;
	esac
else
	SRC="$(norm "$SRC")"
	[ -e "$SRC" ] || die "source not found:" "$SRC"
fi

# the file that shows the daemon is serving
if [ "$MODE" = view ]; then READY=shared.vhd; else READY=share.vhd; fi

# one instance only
if [ -f "$PIDFILE" ]; then
	OLD="$(cat "$PIDFILE" 2>/dev/null)"
	# a live tsshare, whatever its mount point, is never called stale
	if [ -n "$(ts_mount "$OLD")" ]; then
		echo "tsshare is already running (pid $OLD)."
		echo "Run tsshare_stop.sh first to change the source."
		exit 1
	fi
	echo "Removed stale pid file (pid $OLD is not tsshare)."
	rm -f "$PIDFILE"
fi
OLD="$(scan_any)"
if [ -n "$OLD" ]; then
	echo "tsshare is already running (pid $OLD)."
	echo "Run tsshare_stop.sh first."
	exit 1
fi
# a mount left behind by a dead daemon (ours only)
if is_mounted; then
	echo "Clearing a leftover tsshare mount."
	lazy_umount "$MOUNT" || die "cannot unmount" "$MOUNT"
fi
# something else is mounted there: never touch it
if [ -n "$(mount_type)" ]; then
	die "another filesystem is mounted at" "$MOUNT"
fi

# fusectl makes the abort recipe possible (README); harmless if present
if [ -d /sys/fs/fuse/connections ] && \
	[ -z "$(ls /sys/fs/fuse/connections 2>/dev/null)" ] && \
	! grep -q " /sys/fs/fuse/connections " /proc/mounts 2>/dev/null; then
	mount -t fusectl none /sys/fs/fuse/connections 2>/dev/null
fi

if [ -d "$MOUNT" ] && [ -n "$(ls -A "$MOUNT" 2>/dev/null)" ]; then
	echo "Note: files in MOUNT are hidden while mounted" | cut -c1-60
fi
if [ "$MODE" = view ] && [ ! -d "$SHARED" ]; then
	mkdir -p "$SHARED" || die "cannot create" "$SHARED"
	echo "Created folder: ${SHARED##*/}" | cut -c1-60
fi
if [ "$MODE" = view ] && [ ! -d "$MOUNT" ]; then
	mkdir -p "$MOUNT" || die "cannot create" "$MOUNT"
	echo "Created folder: ${MOUNT##*/}" | cut -c1-60
fi
mkdir -p "$MOUNT" || die "cannot create" "$MOUNT"
mkdir -p "$CONF_DIR" || die "cannot create" "$CONF_DIR"
# without a log file the daemon would never start: say why (read-only or full SD card?)
( : > "$LOG" ) 2>/dev/null || die "cannot write the log (SD full/read-only?)" "$LOG"

if [ "$MODE" = view ]; then
	echo "Viewing: ${ZIPROOT##*/}" | cut -c1-60
	echo "Starting (up to ${WAIT_SECS}s)..."
	setsid nohup "$BIN" -view "$ZIPROOT" -shared "$SHARED" -mount "$MOUNT" \
		-cache-mb "$CACHE_MB" -label "$LABEL" > "$LOG" 2>&1 < /dev/null &
else
	echo "Sharing: ${SRC##*/}" | cut -c1-60
	echo "Scanning (up to ${WAIT_SECS}s)..."
	setsid nohup "$BIN" -src "$SRC" -mount "$MOUNT" -cache-mb "$CACHE_MB" \
		-label "$LABEL" > "$LOG" 2>&1 < /dev/null &
fi
PID=$!
echo "$PID" > "$PIDFILE"

# Ctrl-C only stops this script; the daemon (setsid) keeps going
trap 'echo; echo "Interrupted. The daemon may still be"; echo "starting: run tsshare_stop.sh."; exit 130' INT TERM

i=0
while ! ready; do
	if ! alive "$PID"; then
		# setsid may have forked: look for the real daemon
		REAL="$(scan_pid)"
		if [ -n "$REAL" ]; then
			PID="$REAL"
			echo "$PID" > "$PIDFILE"
		else
			rm -f "$PIDFILE"
			echo "FAILED: the daemon exited."
			show_log
			exit 1
		fi
	fi
	if [ "$i" -ge "$WAIT_SECS" ]; then
		echo "FAILED: no $READY after ${WAIT_SECS}s."
		show_log
		echo "Run tsshare_stop.sh to clean up."
		exit 1
	fi
	sleep 1
	i=$((i + 1))
done
trap - INT TERM

if [ "$MODE" = view ]; then
	echo "View ready: pick in the OSD (${i}s, pid $PID)"
	echo "Mount SD2 image -> ${MOUNT##*/} ->" | cut -c1-60
	echo "  <name>.vhd or shared.vhd"
	echo "Log: $(short "$LOG")"
	exit 0
fi
echo "OK, ready after ${i}s (pid $PID)"
grep '^tree:' "$LOG" | sed 's/^tree: //; s/ (including.*//' | cut -c1-60
grep '^volume:' "$LOG" | sed 's/^volume: //; s/, partition.*//' | cut -c1-60
grep -E '^[0-9]+ warnings?$' "$LOG"
echo "In the OSD: Mount SD2 image ->"
echo "  tsshare -> share.vhd, then reset."
echo "Log: $(short "$LOG")"
exit 0
