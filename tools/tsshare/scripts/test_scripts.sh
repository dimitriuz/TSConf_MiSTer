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
HELPERS=""
cleanup() {
	trap '' INT TERM
	# the daemon, by the pid file the scripts use
	if [ -f "$T/conf/tsshare.pid" ]; then
		kill -KILL "$(cat "$T/conf/tsshare.pid")" 2>/dev/null
	fi
	for h in $HELPERS; do kill -KILL "$h" 2>/dev/null; done
	fusermount3 -uz "$MOUNT" 2>/dev/null
	fusermount3 -uz "$T/vroot/share" 2>/dev/null
	sleep 1
	rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

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

# the older tests below exercise the single-source mode; view tests set MODE=view
export MODE=single
export MOUNT BIN T
export CONF_DIR="$T/conf" LOG="$T/conf/tsshare.log" PIDFILE="$T/conf/tsshare.pid"
export CACHE_MB=8 WAIT_SECS=20
START="$HERE/tsshare_start.sh"
STOP="$HERE/tsshare_stop.sh"
export STOP
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

# lib blocks must be identical in both scripts
sed -n '/BEGIN lib/,/END lib/p' "$START" > "$T/lib.start"
sed -n '/BEGIN lib/,/END lib/p' "$STOP" > "$T/lib.stop"
expect "lib blocks in sync" cmp -s "$T/lib.start" "$T/lib.stop"
expect "lib block found" test -s "$T/lib.start"
sed -n '/BEGIN defaults/,/END defaults/p' "$START" > "$T/def.start"
sed -n '/BEGIN defaults/,/END defaults/p' "$STOP" > "$T/def.stop"
expect "defaults blocks in sync" cmp -s "$T/def.start" "$T/def.stop"
expect "defaults block found" test -s "$T/def.start"

# mount matcher against sample /proc/mounts lines
matcher() { # matcher MOUNT MOUNTS_FILE -> sets nothing, prints yes/no
	(MOUNT="$1" MOUNTS_FILE="$2"; . "$T/lib.start"; is_mounted && echo yes || echo no)
}
cat > "$T/m1" <<'M'
sshfs#u@h:/x /media/fat/tsshare fuse.sshfs rw 0 0
M
cat > "$T/m2" <<'M'
tsshare /media/fat/tsshare2 fuse.tsshare rw 0 0
M
cat > "$T/m3" <<'M'
tsshare /media/fat/tsshare fuse.tsshare rw 0 0
M
cat > "$T/m4" <<'M'
tsshare /a\040b/ts fuse.tsshare rw 0 0
M
cat > "$T/m5" <<'M'
tsshare /media/fat/tsshare fuse.tsshare rw 0 0
sshfs#u@h:/x /media/fat/tsshare fuse.sshfs rw 0 0
M
cat > "$T/m6" <<'M'
tsshare /media/fat/tsshare fuse rw 0 0
M
expect "foreign fuse.sshfs not ours" test "$(matcher /media/fat/tsshare "$T/m1")" = no
expect "prefix path (tsshare2) not matched" test "$(matcher /media/fat/tsshare "$T/m2")" = no
expect "fuse.tsshare matched" test "$(matcher /media/fat/tsshare "$T/m3")" = yes
expect "octal-escaped space matched" test "$(matcher "/a b/ts" "$T/m4")" = yes
expect "escaped path, wrong dir" test "$(matcher "/a/b/ts" "$T/m4")" = no
expect "foreign mount stacked on top" test "$(matcher /media/fat/tsshare "$T/m5")" = no
expect "plain fstype fuse not ours" test "$(matcher /media/fat/tsshare "$T/m6")" = no
# foreign mount at MOUNT: start refuses, stop leaves it alone
sed "s|/media/fat/tsshare|$MOUNT|" "$T/m1" > "$T/mf"
check "start over a foreign mount" 1 -- env MOUNTS_FILE="$T/mf" sh "$START" "$T/src"
expect "foreign mount message" has "another filesystem"
check "stop with a foreign mount" 0 -- env MOUNTS_FILE="$T/mf" sh "$STOP"
expect "foreign mount left alone" has "not running"

# unrelated live process in the pid file must survive
sleep 100 &
UNREL=$!
HELPERS="$HELPERS $UNREL"
echo "$UNREL" > "$PIDFILE"
check "stop with pid file of an unrelated process" 0 -- sh "$STOP"
expect "stale reported" has "stale"
expect "unrelated process still alive" kill -0 "$UNREL"
expect "pid file removed (unrelated)" no_pidfile
# a process whose argv[0] is tsshare but without -mount
mkdir -p "$T/fake"
ln -s "$(command -v sleep)" "$T/fake/tsshare"
"$T/fake/tsshare" 100 &
FAKE=$!
HELPERS="$HELPERS $FAKE"
grep -q tsshare "/proc/$FAKE/cmdline" || echo "note: fake cmdline lacks tsshare"
echo "$FAKE" > "$PIDFILE"
check "stop with a tsshare-named process without -mount" 0 -- sh "$STOP"
expect "stale reported (fake)" has "stale"
expect "fake process still alive" kill -0 "$FAKE"
echo "$FAKE" > "$PIDFILE"
check "start with that pid file" 0 -- sh "$START" "$T/src"
expect "start called it stale" has "stale"
check "stop after fake" 0 -- sh "$STOP"
expect "fake survived again" kill -0 "$FAKE"
kill -KILL "$UNREL" "$FAKE" 2>/dev/null

# processes that must never be signalled. Symlinks to sh give argv[0]
# basenames; "-c LOOP" then positional words, so the cmdline holds the words.
mkdir -p "$T/fake2"
ln -s "$(command -v sh)" "$T/fake2/tsshare"
ln -s "$(command -v sh)" "$T/fake2/other"
LOOP='while :; do sleep 1; done'
spare() { # spare NAME_IN_fake2 WORDS... : runs it in the background, sets FP
	_n="$1"; shift
	"$T/fake2/$_n" -c "$LOOP" "$@" &
	FP=$!
	HELPERS="$HELPERS $FP"
}
survives() { # survives LABEL PID: stop must call the pid file stale and spare it
	echo "$2" > "$PIDFILE"
	check "stop spares: $1" 0 -- sh "$STOP"
	expect "$1: reported stale" has "stale"
	expect "$1: still alive" kill -0 "$2"
	kill -KILL "$2" 2>/dev/null
}
spare other -mount "$MOUNT"
survives "argv0 is not tsshare (other -mount MOUNT)" "$FP"
spare tsshare x "$MOUNT"
survives "tsshare MOUNT without -mount" "$FP"
spare tsshare "-mount=$MOUNT"
survives "-mount=MOUNT form (start always uses the space form)" "$FP"
# the stop script's own parent, dressed up as the daemon
rm -f "$T/ppid.alive"
"$T/fake2/tsshare" -c 'echo $$ > "$PIDFILE"; sh "$STOP" > "$T/ppid.out" 2>&1; echo done > "$T/ppid.alive"' -mount "$MOUNT"
expect "stop did not kill its parent" test -f "$T/ppid.alive"
expect "parent: reported stale" grep -q stale "$T/ppid.out"

# BIN must be named tsshare
cp "$BIN" "$T/tsshare_bin"
check "BIN with another name" 1 -- env BIN="$T/tsshare_bin" sh "$START" "$T/src"
expect "BIN name message" has "must be named"
expect "nothing started (no pid file)" no_pidfile
expect "nothing mounted" not_mounted
# MOUNT with trailing slashes, and refused values
check "start with MOUNT/ (trailing slash)" 0 -- env MOUNT="$MOUNT//" sh "$START" "$T/src"
expect "mounted at the clean path" mounted
check "stop with MOUNT/" 0 -- env MOUNT="$MOUNT/" sh "$STOP"
expect "unmounted after stop with MOUNT/" not_mounted
check "MOUNT=/ refused" 1 -- env MOUNT=/ sh "$START" "$T/src"
check "MOUNT=/ refused by stop" 1 -- env MOUNT=/ sh "$STOP"
# a very long source name stays within 60 columns
LN="$(printf 'n%.0s' $(seq 1 90))"
mkdir "$T/$LN"
check "long source name" 0 -- sh "$START" "$T/$LN"
expect "long name: no line over 60 columns" test -z "$(echo "$LAST" | grep -v '^Log:' | awk 'length($0) > 60')"
sh "$STOP" > /dev/null 2>&1

# output width: no line longer than 60 columns (paths of the temp dir excluded)
check "width probe" 0 -- sh "$START" "$T/src"
LONG="$(echo "$LAST" | grep -v "^Log:" | awk 'length($0) > 60')"
expect "no output line over 60 columns" test -z "$LONG"
sh "$STOP" > /dev/null 2>&1

check "stop with nothing running" 0 -- sh "$STOP"
check "bad SRC" 1 -- sh "$START" "$T/nonexistent"
expect "bad SRC message" has "source not found"
check "missing BIN" 1 -- env BIN="$T/nb/tsshare" sh "$START" "$T/src"
expect "missing BIN message" has "no daemon"
: > "$T/notexec"
mkdir -p "$T/ne"; : > "$T/ne/tsshare"
check "non-executable BIN" 1 -- env BIN="$T/ne/tsshare" sh "$START" "$T/src"
expect "non-executable message" has "not executable"

check "unwritable LOG" 1 -- env LOG="$T/no/such/dir/tsshare.log" sh "$START" "$T/src"
expect "unwritable LOG message" has "read-only or full SD card"
not_mounted_ok() { not_mounted && no_pidfile; }
expect "unwritable LOG leaves nothing running" not_mounted_ok

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

# ---- view mode (the default): a zip root, a shared folder ----
mkzip() { # mkzip FILE : a small zip with one 5000-byte entry
	if command -v python3 >/dev/null 2>&1; then
		python3 -I -c 'import sys,zipfile
z=zipfile.ZipFile(sys.argv[1],"w",zipfile.ZIP_DEFLATED)
z.writestr("dir/x.txt","x"*5000)
z.close()' "$1"
	elif command -v zip >/dev/null 2>&1; then
		( cd "$T" && echo x > vx.txt && zip -q "$1" vx.txt )
	else
		return 1
	fi
}
VMOUNT="$T/vroot/share"
vmounted() { grep -q " $VMOUNT fuse" /proc/mounts; }
not_vmounted() { ! vmounted; }
no_zip_name() { ! ls "$VMOUNT" "$VMOUNT"/* 2>/dev/null | grep -q '\.zip'; }
mkdir -p "$T/vroot/sub"
if mkzip "$T/vroot/game.zip" && mkzip "$T/vroot/sub/inner.zip"; then
	VENV="MODE=view ZIPROOT=$T/vroot SHARED=$T/vroot/shared MOUNT=$VMOUNT"
	check "view: start (creates shared and mount)" 0 -- env $VENV sh "$START"
	expect "view: summary" has "^View ready: pick in the OSD"
	expect "view: OSD hint" has "Mount SD2 image -> share ->"
	expect "view: OSD names" has "<name>.vhd or shared.vhd"
	expect "view: created shared" has "Created folder: shared"
	expect "view: created mount" has "Created folder: share"
	expect "view: no line over 60 columns" test -z "$(echo "$LAST" | grep -v '^Log:' | awk 'length($0) > 60')"
	expect "view: mounted" vmounted
	expect "view: pid file written" test -s "$PIDFILE"
	expect "view: shared.vhd" test -e "$VMOUNT/shared.vhd"
	expect "view: game.vhd" test -s "$VMOUNT/game.vhd"
	expect "view: sub/inner.vhd" test -s "$VMOUNT/sub/inner.vhd"
	expect "view: no entry contains .zip" no_zip_name
	expect "view: mount point itself hidden" test ! -e "$VMOUNT/share"
	check "view: second start refuses" 1 -- env $VENV sh "$START"
	expect "view: refusal message" has "already running"
	expect "view: still mounted after refusal" vmounted
	check "view: stop" 0 -- env $VENV sh "$STOP"
	expect "view: unmounted" not_vmounted
	expect "view: pid file removed" no_pidfile
	# existing folders are not announced; a file in shared is served
	echo hi > "$T/vroot/shared/note.txt"
	check "view: restart" 0 -- env $VENV sh "$START"
	expect "view: nothing created the second time" test -z "$(echo "$LAST" | grep Created)"
	expect "view: shared.vhd after restart" test -e "$VMOUNT/shared.vhd"
	check "view: stop (restart)" 0 -- env $VENV sh "$STOP"
	expect "view: unmounted (restart)" not_vmounted
	check "view: stop with nothing running" 0 -- env $VENV sh "$STOP"
	# argument = zip root
	check "view: zip root as argument" 0 -- env MODE=view SHARED="$T/vroot/shared" MOUNT="$VMOUNT" sh "$START" "$T/vroot"
	check "view: stop (argument)" 0 -- env $VENV sh "$STOP"
	# a mount inside the shared folder is refused, nothing left behind
	check "view: MOUNT inside SHARED refused" 1 -- env MODE=view ZIPROOT="$T/vroot" SHARED="$T/vroot/shared" MOUNT="$T/vroot/shared/m" sh "$START"
	expect "view: inside-SHARED message" has "inside SHARED"
	expect "view: inside-SHARED leaves no pid file" no_pidfile
	# the daemon refuses a symlinked way in: its message reaches the log tail
	ln -s "$T/vroot/shared" "$T/vlink"
	check "view: MOUNT inside SHARED via symlink (daemon check)" 1 -- env MODE=view ZIPROOT="$T/vroot" SHARED="$T/vlink" MOUNT="$T/vroot/shared/m2" sh "$START"
	expect "view: daemon check leaves no pid file" no_pidfile
	rm -rf "$T/vroot/shared/m" "$T/vroot/shared/m2"
else
	echo "skip: view tests (no python3 or zip)"
fi
check "view: missing ZIPROOT" 1 -- env MODE=view ZIPROOT="$T/nozip" SHARED="$T/nozip/shared" MOUNT="$T/nozip/share" sh "$START"
expect "view: missing ZIPROOT message" has "folder not found"
expect "view: missing ZIPROOT created nothing" test ! -e "$T/nozip"
expect "view: missing ZIPROOT: no pid file" no_pidfile
check "bad MODE" 1 -- env MODE=bogus sh "$START"
expect "bad MODE message" has "MODE must be"
check "bad MODE (stop)" 1 -- env MODE=bogus sh "$STOP"
# MODE=single is unchanged (the tests above all run in it)
check "MODE=single start" 0 -- env MODE=single sh "$START" "$T/src"
expect "MODE=single share.vhd" test -s "$MOUNT/share.vhd"
check "MODE=single stop" 0 -- env MODE=single sh "$STOP"
expect "MODE=single unmounted" not_mounted

echo
if [ "$FAILS" = 0 ]; then echo "ALL PASSED"; exit 0; fi
echo "$FAILS FAILED"; exit 1
