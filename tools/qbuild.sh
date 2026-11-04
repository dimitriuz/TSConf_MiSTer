#!/usr/bin/env bash
# Build a Quartus project in Docker and print the resulting RBF.
# Usage: tools/qbuild.sh <project-dir> <revision>
# The project dir is relative to the current directory; it is mounted read-write
# into the image at /work. Env overrides:
#   QUARTUS_IMAGE  image to run (default: raetro/quartus:17.0)
#   QUARTUS_SH     quartus_sh inside the image (default: quartus_sh, which is on
#                  the image PATH; both images carry it)
# Exits with Quartus' exit code. The last line is always `qbuild: exit <code>`
# (printed from an EXIT trap, so even the usage error ends with it) so a detached
# poller can see the outcome in the log. The container is named qbuild-$$;
# docker run executes in the background and the script wait()s on it, so INT/TERM
# take effect immediately: the traps `docker rm -f` the container (killing the
# build even when the process inside ignores signals) and exit 130/143. An RBF
# that is not newer than the build start is an error: a leftover
# output_files/*.rbf can never pass as this build's output.
set -euo pipefail

wall=0
start=0
cname=""
remove_container() {
    if [ -n "$cname" ] && docker ps -q --filter "name=^${cname}$" | grep -q .; then
        docker rm -f "$cname" >/dev/null 2>&1 || true
    fi
    return 0
}
cleanup() {
    local c=$?
    remove_container
    if [ "$start" -ne 0 ]; then
        wall=$(( $(date +%s) - start ))
    fi
    echo "qbuild: wall ${wall}s"
    echo "qbuild: exit $c"
}
trap cleanup EXIT
trap 'remove_container; exit 130' INT
trap 'remove_container; exit 143' TERM

if [ $# -ne 2 ]; then
    echo "usage: tools/qbuild.sh <project-dir> <revision>" >&2
    exit 2
fi
proj="$1"
rev="$2"

: "${QUARTUS_IMAGE:=raetro/quartus:17.0}"
: "${QUARTUS_SH:=quartus_sh}"
if [ ! -d "$proj" ]; then
    echo "qbuild: no such project dir: $proj" >&2
    exit 2
fi

cname="qbuild-$$"
start=$(date +%s)
start_ns=$(date +%s%N)
set +e
docker run --rm --name "$cname" \
    -v "$PWD":/work -w "/work/$proj" \
    -u "$(id -u):$(id -g)" -e HOME=/tmp \
    "$QUARTUS_IMAGE" \
    "$QUARTUS_SH" --flow compile "$rev" &
wait "$!"
code=$?
set -e

if [ "$code" -eq 0 ]; then
    rbf="$proj/output_files/$rev.rbf"
    rbf_ns=$(stat -c %.9Y "$rbf" 2>/dev/null | tr -d . || true)
    rbf_ns=${rbf_ns:-0}
    if [ ! -f "$rbf" ]; then
        echo "qbuild: ERROR: quartus_sh exit 0 but $rbf missing" >&2
        code=1
    elif [ "$rbf_ns" -le "$start_ns" ]; then
        echo "qbuild: ERROR: $rbf is not newer than the build start ($rbf_ns <= $start_ns) - stale output" >&2
        code=1
    else
        echo "qbuild: $rbf $(stat -c %s "$rbf") bytes"
    fi
fi
exit "$code"
