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
# -----------------------------------------------------------------------

[ -n "$1" ] && SRC="$1"

die() {
	echo "ERROR: $*"
	exit 1
}

is_mounted() {
	grep -q " $MOUNT fuse" /proc/mounts 2>/dev/null
}

# lazy unmount: root uses umount -l (the board has no fusermount),
# a normal user (host tests) uses fusermount3 -uz
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

# is $1 a live tsshare process?
is_ours() {
	[ -n "$1" ] && alive "$1" && \
		grep -q tsshare "/proc/$1/cmdline" 2>/dev/null
}

# pid of a tsshare daemon for $MOUNT found in ps output (BusyBox or procps)
scan_pid() {
	ps w 2>/dev/null | grep tsshare | grep -v grep | grep -e "-mount $MOUNT" |
		while read -r p rest; do
			echo "$p"
			break
		done
}

show_log() {
	[ -f "$LOG" ] && tail -n 4 "$LOG" | fold -w 60
}

[ -f "$BIN" ] || die "no daemon at $BIN"
[ -x "$BIN" ] || die "$BIN is not executable (chmod +x)"
[ -e "$SRC" ] || die "source not found: $SRC"

# one instance only
if [ -f "$PIDFILE" ]; then
	OLD="$(cat "$PIDFILE" 2>/dev/null)"
	if is_ours "$OLD"; then
		echo "tsshare is already running (pid $OLD)."
		echo "Run tsshare_stop.sh first to change the source."
		exit 1
	fi
	echo "Removing stale pid file."
	rm -f "$PIDFILE"
fi
OLD="$(scan_pid)"
if [ -n "$OLD" ]; then
	echo "tsshare is already running (pid $OLD)."
	echo "Run tsshare_stop.sh first."
	exit 1
fi
# a mount left behind by a dead daemon
if is_mounted; then
	echo "Clearing a leftover mount at $MOUNT"
	lazy_umount "$MOUNT" || die "cannot unmount $MOUNT"
fi

# fusectl makes the abort recipe possible (README); harmless if present
if [ -d /sys/fs/fuse/connections ] && \
	[ -z "$(ls /sys/fs/fuse/connections 2>/dev/null)" ] && \
	! grep -q " /sys/fs/fuse/connections " /proc/mounts 2>/dev/null; then
	mount -t fusectl none /sys/fs/fuse/connections 2>/dev/null
fi

mkdir -p "$MOUNT" || die "cannot create $MOUNT"
mkdir -p "$CONF_DIR" || die "cannot create $CONF_DIR"

echo "Sharing: $SRC"
echo "Scanning (up to ${WAIT_SECS}s)..."
setsid nohup "$BIN" -src "$SRC" -mount "$MOUNT" -cache-mb "$CACHE_MB" \
	-label "$LABEL" > "$LOG" 2>&1 < /dev/null &
PID=$!
echo "$PID" > "$PIDFILE"

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

echo "OK, ready after ${i}s (pid $PID)"
grep '^tree:' "$LOG" | sed 's/^tree: //; s/ (including.*//'
grep '^volume:' "$LOG" | sed 's/^volume: //; s/, partition.*//' | cut -c1-60
grep -E '^[0-9]+ warnings?$' "$LOG"
echo "In the OSD: Mount SD2 image ->"
echo "  tsshare -> share.vhd, then reset."
echo "Log: $LOG"
exit 0
