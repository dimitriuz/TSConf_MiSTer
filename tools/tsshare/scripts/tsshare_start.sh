#!/bin/sh
# tsshare_start.sh - start the tsshare daemon (MiSTer Scripts menu).
#
# Serves a folder or a zip as a read-only FAT32 disk, share.vhd, in
# $MOUNT. Then, in the TSConf OSD: Mount SD2 image -> tsshare -> share.vhd.
# Usage: tsshare_start.sh [folder-or-zip]
# Touches only $MOUNT (creates it) and the files in $CONF_DIR.

# ---- settings (edit here, or override with environment variables) ----
SRC="${SRC:-/media/fat/games/Spectrum/compilations/AAA/SD.zip}"
MOUNT="${MOUNT:-/media/fat/tsshare}"
CONF_DIR="${CONF_DIR:-/media/fat/Scripts/.config/tsshare}"
BIN="${BIN:-$CONF_DIR/tsshare}"
CACHE_MB="${CACHE_MB:-32}"
LABEL="${LABEL:-TSSHARE}"
# ---- test hooks (used by test_scripts.sh; leave alone on the board) ----
LOG="${LOG:-$CONF_DIR/tsshare.log}"
PIDFILE="${PIDFILE:-$CONF_DIR/tsshare.pid}"
WAIT_SECS="${WAIT_SECS:-90}"
# MOUNTS_FILE: see the lib below
# -----------------------------------------------------------------------

[ -n "$1" ] && SRC="$1"

die() {
	echo "ERROR: $1"
	[ -n "$2" ] && echo "  $2"
	exit 1
}

# ---- BEGIN lib (identical in start and stop; test_scripts.sh diffs it) ----
# MOUNTS_FILE is a test hook (a sample /proc/mounts).

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
	tr '\0' '\n' < "/proc/$_p/cmdline" 2>/dev/null | {
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
# ---- END lib ----

show_log() {
	[ -f "$LOG" ] && tail -n 4 "$LOG" | cut -c1-60
}

[ -f "$BIN" ] || die "no daemon at" "$BIN"
[ -x "$BIN" ] || die "not executable (chmod +x):" "$BIN"
[ -e "$SRC" ] || die "source not found:" "$SRC"

# one instance only
if [ -f "$PIDFILE" ]; then
	OLD="$(cat "$PIDFILE" 2>/dev/null)"
	if is_ours "$OLD"; then
		echo "tsshare is already running (pid $OLD)."
		echo "Run tsshare_stop.sh first to change the source."
		exit 1
	fi
	echo "Removed stale pid file (pid $OLD is not tsshare)."
	rm -f "$PIDFILE"
fi
OLD="$(scan_pid)"
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

mkdir -p "$MOUNT" || die "cannot create" "$MOUNT"
mkdir -p "$CONF_DIR" || die "cannot create" "$CONF_DIR"

echo "Sharing: ${SRC##*/}"
echo "Scanning (up to ${WAIT_SECS}s)..."
setsid nohup "$BIN" -src "$SRC" -mount "$MOUNT" -cache-mb "$CACHE_MB" \
	-label "$LABEL" > "$LOG" 2>&1 < /dev/null &
PID=$!
echo "$PID" > "$PIDFILE"

# Ctrl-C only stops this script; the daemon (setsid) keeps going
trap 'echo; echo "Interrupted. The daemon may still be"; echo "starting: run tsshare_stop.sh."; exit 130' INT TERM

i=0
while [ ! -e "$MOUNT/share.vhd" ]; do
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
		echo "FAILED: no share.vhd after ${WAIT_SECS}s."
		show_log
		echo "Run tsshare_stop.sh to clean up."
		exit 1
	fi
	sleep 1
	i=$((i + 1))
done
trap - INT TERM

echo "OK, ready after ${i}s (pid $PID)"
grep '^tree:' "$LOG" | sed 's/^tree: //; s/ (including.*//' | cut -c1-60
grep '^volume:' "$LOG" | sed 's/^volume: //; s/, partition.*//' | cut -c1-60
grep -E '^[0-9]+ warnings?$' "$LOG"
echo "In the OSD: Mount SD2 image ->"
echo "  tsshare -> share.vhd, then reset."
echo "Log: $LOG"
exit 0
