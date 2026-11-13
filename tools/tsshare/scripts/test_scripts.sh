#!/bin/sh
# Host dry run of tsshare_start.sh / tsshare_stop.sh (non-root, fusermount3).
# Builds the daemon (needs go) unless BIN points at one. Everything happens
# under a temp dir; mounts are always removed on exit.
# Usage: scripts/test_scripts.sh      (TMPDIR is honoured)

HERE="$(cd "$(dirname "$0")" && pwd)"
TOP="$(dirname "$HERE")"

[ -e /dev/fuse ] || { echo "SKIP: no /dev/fuse"; exit 0; }
command -v fusermount3 >/dev/null 2>&1 || { echo "SKIP: no fusermount3"; exit 0; }

T="$(mktemp -d "${TMPDIR:-/tmp}/tsshare-scripts.XXXXXX")" || exit 1
MOUNT="$T/mnt"
cleanup() {
	fusermount3 -uz "$MOUNT" 2>/dev/null
	for f in "$T"/pid* ; do
		[ -f "$f" ] && kill -KILL "$(cat "$f")" 2>/dev/null
	done
	sleep 1
	rm -rf "$T"
}
trap cleanup EXIT

if [ -z "$BIN" ]; then
	BIN="$T/tsshare"
	echo "building daemon..."
	(cd "$TOP" && go build -o "$BIN" .) || { echo "FAIL: build"; exit 1; }
fi
mkdir -p "$T/src/sub" "$T/conf"
echo hello > "$T/src/a.txt"
echo world > "$T/src/sub/b.txt"
if command -v python3 >/dev/null 2>&1; then
	python3 -I -c 'import sys,zipfile
z=zipfile.ZipFile(sys.argv[1],"w",zipfile.ZIP_DEFLATED)
z.writestr("dir/x.txt","x"*5000)
z.close()' "$T/test.zip"
fi

export MOUNT BIN
export CONF_DIR="$T/conf" LOG="$T/conf/tsshare.log" PIDFILE="$T/conf/tsshare.pid"
export CACHE_MB=8 WAIT_SECS=20
START="$HERE/tsshare_start.sh"
STOP="$HERE/tsshare_stop.sh"
FAILS=0

# check NAME EXPECTED_STATUS -- command...
check() {
	name="$1"; want="$2"; shift 3
	out="$("$@" 2>&1)"
	got=$?
	echo "--- $name (exit $got, want $want)"
	echo "$out" | sed 's/^/    /'
	LAST="$out"
	if [ "$got" != "$want" ]; then
		echo "FAIL: $name"
		FAILS=$((FAILS + 1))
	fi
}
expect() { # expect NAME CONDITION-COMMAND...
	name="$1"; shift
	if "$@"; then echo "ok: $name"; else echo "FAIL: $name"; FAILS=$((FAILS + 1)); fi
}
mounted() { grep -q " $MOUNT fuse" /proc/mounts; }
not_mounted() { ! mounted; }
no_pidfile() { [ ! -e "$PIDFILE" ]; }
has() { echo "$LAST" | grep -q "$1"; }

check "stop with nothing running" 0 -- sh "$STOP"
check "bad SRC" 1 -- sh "$START" "$T/nonexistent"
expect "bad SRC message" has "source not found"
check "missing BIN" 1 -- env BIN="$T/nobin" sh "$START" "$T/src"
expect "missing BIN message" has "no daemon"
: > "$T/notexec"
check "non-executable BIN" 1 -- env BIN="$T/notexec" sh "$START" "$T/src"
expect "non-executable message" has "not executable"

check "start (folder)" 0 -- sh "$START" "$T/src"
expect "summary has OK" has "^OK"
expect "summary has geometry" has "sectors/cluster"
expect "summary has warnings" has "warnings"
expect "summary has OSD path" has "share.vhd"
expect "share.vhd visible" test -s "$MOUNT/share.vhd"
expect "pid file written" test -s "$PIDFILE"
check "second start refuses" 1 -- sh "$START" "$T/src"
expect "refusal message" has "already running"
expect "still mounted after refusal" mounted
check "stop" 0 -- sh "$STOP"
expect "unmounted" not_mounted
expect "pid file removed" no_pidfile
check "stop again" 0 -- sh "$STOP"
expect "not running message" has "not running"

if [ -f "$T/test.zip" ]; then
	check "start (zip, argument via SRC env)" 0 -- env SRC="$T/test.zip" sh "$START"
	expect "zip share.vhd visible" test -s "$MOUNT/share.vhd"
	check "stop (zip)" 0 -- sh "$STOP"
	expect "unmounted (zip)" not_mounted
else
	echo "skip: zip case (no python3)"
fi

# stale pid file: the process is dead, the number is bogus
sleep 0 & DEAD=$!
wait "$DEAD"
echo "$DEAD" > "$PIDFILE"
check "start with stale pid file" 0 -- sh "$START" "$T/src"
expect "stale pid message" has "stale"
expect "share.vhd visible after stale" test -s "$MOUNT/share.vhd"
check "stop after stale" 0 -- sh "$STOP"

# daemon killed with -9: dead mount and stale pid file left behind
check "start before kill -9" 0 -- sh "$START" "$T/src"
kill -KILL "$(cat "$PIDFILE")"
sleep 1
check "start over a dead mount" 0 -- sh "$START" "$T/src"
expect "leftover mount message" has "leftover"
check "stop (final)" 0 -- sh "$STOP"
expect "unmounted (final)" not_mounted

# daemon that exits at once (bad zip)
echo "not a zip" > "$T/bad.zip"
check "start with a bad zip" 1 -- sh "$START" "$T/bad.zip"
expect "bad zip message" has "FAILED"
expect "pid file removed after failure" no_pidfile
check "stop after failure" 0 -- sh "$STOP"

echo
if [ "$FAILS" = 0 ]; then echo "ALL PASSED"; exit 0; fi
echo "$FAILS FAILED"; exit 1
