#!/bin/sh
# tsshare_stop.sh - stop the tsshare daemon (MiSTer Scripts menu).
# Touches only $MOUNT (unmounts, keeps the directory) and the pid file.
# The running daemon is found whatever MODE or MOUNT say.

# ---- settings (override with environment variables) ----
MODE="${MODE:-view}"
CONF_DIR="${CONF_DIR:-/media/fat/Scripts/.config/tsshare}"
# ---- test hooks (used by test_scripts.sh; leave alone on the board) ----
PIDFILE="${PIDFILE:-$CONF_DIR/tsshare.pid}"
WAIT_SECS="${WAIT_SECS:-10}"
# MOUNTS_FILE: see the lib below
# MOUNT defaults: view /media/fat/games/TSConf/share, single /media/fat/tsshare.
# The daemon is found by its own command line, so MODE and MOUNT only
# matter for cleaning up a mount whose daemon is dead.
# SCAN_PREFIX: see the lib.
# --------------------------------------------------------

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

# wait up to $1 seconds until the process is gone and the mount is gone
wait_down() {
	n=0
	while [ "$n" -lt "$1" ]; do
		if ! alive "$PID" && ! is_mounted; then
			return 0
		fi
		sleep 1
		n=$((n + 1))
	done
	! alive "$PID" && ! is_mounted
}

echo "If SD2 is still mounted in the OSD, eject"
echo "it or reset the core first."

# Find the daemon whatever MODE or MOUNT say: the pid file first, then a
# process scan. A live process whose argv[0] is tsshare and that has
# "-mount <dir>" is adopted with that dir; anything else in the pid file
# is stale and is never signalled.
PID=""
if [ -f "$PIDFILE" ]; then
	P="$(cat "$PIDFILE" 2>/dev/null)"
	if [ -n "$(ts_mount "$P")" ]; then
		PID="$P"
	else
		echo "Removed stale pid file (pid $P is not tsshare)."
		rm -f "$PIDFILE"
	fi
fi
[ -z "$PID" ] && PID="$(scan_any)"
if [ -n "$PID" ]; then
	M="$(ts_mount "$PID")"
	if [ "$M" != "$MOUNT" ]; then
		echo "Found the daemon mounted at:"
		echo "  $(short "$M")"
		MOUNT="$M"
	fi
fi

if [ -z "$PID" ]; then
	# no daemon: clear a leftover tsshare mount at the default mount
	# (or, with MOUNT not given, at the other mode's default)
	LEFT=""
	for m in "$MOUNT" "$ALT"; do
		[ -n "$m" ] || continue
		MOUNT="$m"
		is_mounted || continue
		LEFT=1
		echo "No daemon, but a tsshare mount is left."
		if lazy_umount "$MOUNT"; then
			echo "Unmounted it (lazy)."
		else
			echo "FAILED to unmount:"
			echo "  $(short "$MOUNT")"
			exit 1
		fi
	done
	[ -n "$LEFT" ] || echo "tsshare is not running."
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
		is_ours "$PID" && kill -TERM "$PID" 2>/dev/null
		if ! wait_down 3; then
			echo "Still alive: sending KILL."
			is_ours "$PID" && kill -KILL "$PID" 2>/dev/null
			sleep 1
		fi
		is_mounted && lazy_umount "$MOUNT"
	fi
	if alive "$PID"; then
		echo "FAILED: pid $PID will not die."
		exit 1
	fi
	is_mounted && echo "WARNING: still mounted"
	echo "Stopped."
fi
rm -f "$PIDFILE"
exit 0
