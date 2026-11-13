#!/bin/sh
# tsshare_stop.sh - stop the tsshare daemon (MiSTer Scripts menu).
# Touches only $MOUNT (unmounts, keeps the directory) and the pid file.

# ---- settings (override with environment variables) ----
MOUNT="${MOUNT:-/media/fat/tsshare}"
CONF_DIR="${CONF_DIR:-/media/fat/Scripts/.config/tsshare}"
# ---- test hooks (used by test_scripts.sh; leave alone on the board) ----
PIDFILE="${PIDFILE:-$CONF_DIR/tsshare.pid}"
WAIT_SECS="${WAIT_SECS:-10}"
# --------------------------------------------------------

is_mounted() {
	grep -q " $MOUNT fuse" /proc/mounts 2>/dev/null
}

lazy_umount() {
	if [ "$(id -u)" = 0 ]; then
		umount -l "$1" 2>/dev/null && return 0
	fi
	if command -v fusermount3 >/dev/null 2>&1; then
		fusermount3 -uz "$1" 2>/dev/null && return 0
	fi
	umount -l "$1" 2>/dev/null
}

is_ours() {
	[ -n "$1" ] && kill -0 "$1" 2>/dev/null && \
		grep -q tsshare "/proc/$1/cmdline" 2>/dev/null
}

scan_pid() {
	ps w 2>/dev/null | grep tsshare | grep -v grep | grep -e "-mount $MOUNT" |
		while read -r p rest; do
			echo "$p"
			break
		done
}

# wait up to $1 seconds until the process is gone and the mount is gone
wait_down() {
	n=0
	while [ "$n" -lt "$1" ]; do
		if ! kill -0 "$PID" 2>/dev/null && ! is_mounted; then
			return 0
		fi
		sleep 1
		n=$((n + 1))
	done
	! kill -0 "$PID" 2>/dev/null && ! is_mounted
}

echo "If SD2 is still mounted in the OSD, eject"
echo "it or reset the core first."

PID=""
if [ -f "$PIDFILE" ]; then
	P="$(cat "$PIDFILE" 2>/dev/null)"
	is_ours "$P" && PID="$P"
fi
[ -z "$PID" ] && PID="$(scan_pid)"

if [ -z "$PID" ]; then
	rm -f "$PIDFILE"
	if is_mounted; then
		echo "No daemon, but $MOUNT is mounted."
		if lazy_umount "$MOUNT"; then
			echo "Unmounted it (lazy)."
		else
			echo "FAILED to unmount $MOUNT"
			exit 1
		fi
	else
		echo "tsshare is not running."
	fi
	exit 0
fi

echo "Stopping tsshare (pid $PID)..."
kill -TERM "$PID" 2>/dev/null
if wait_down "$WAIT_SECS"; then
	echo "Stopped, unmounted."
else
	if is_mounted; then
		echo "Still mounted (busy?): lazy unmount."
		lazy_umount "$MOUNT"
	fi
	if ! wait_down 3; then
		echo "Still alive: sending TERM again."
		kill -TERM "$PID" 2>/dev/null
		if ! wait_down 3; then
			echo "Still alive: sending KILL."
			kill -KILL "$PID" 2>/dev/null
			sleep 1
		fi
		is_mounted && lazy_umount "$MOUNT"
	fi
	if kill -0 "$PID" 2>/dev/null; then
		echo "FAILED: pid $PID will not die."
		exit 1
	fi
	is_mounted && echo "WARNING: $MOUNT is still mounted"
	echo "Stopped."
fi
rm -f "$PIDFILE"
exit 0
