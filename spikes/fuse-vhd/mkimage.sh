#!/bin/sh
# Build a reproducible MBR + FAT32 test image.
# Usage: mkimage.sh <out.img> <size_mib>
# One FAT32 partition at sector 128 (offset 65536), 512-byte clusters.
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <out.img> <size_mib>" >&2; exit 2; }
OUT=$1; MIB=$2
TOTAL=$((MIB * 2048))
PSECT=$((TOTAL - 128))
# FAT32 needs >= 65525 clusters; 512-byte clusters -> >= ~33 MiB partition (plus FAT overhead)
[ "$PSECT" -ge 140000 ] || { echo "size too small for FAT32 with 512-byte clusters (min ~70 MiB)" >&2; exit 1; }
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT

# deterministic pseudo-random bytes: AES-CTR with fixed key/iv
rnd() { # name bytes
    head -c "$2" /dev/zero | openssl enc -aes-128-ctr -K 00112233445566778899aabbccddeeff -iv "$(printf '%032x' "$3")" -nosalt 2>/dev/null > "$1"
}
mkdir "$T/src" "$T/src/DEMOS"
i=1
for spec in GAME1.trd:65536 TUNE.pt3:98304 DEMO.spg:262144 DISK2.trd:655360 MUSIC2.pt3:131072 DEMOS/INTRO.spg:327680 DEMOS/SMALL.trd:70000; do
    rnd "$T/src/${spec%%:*}" "${spec##*:}" "$i"; i=$((i+1))
done
find "$T/src" -exec touch -d '2026-01-01 00:00:00 UTC' {} +

truncate -s 0 "$OUT"; truncate -s "${MIB}M" "$OUT"
truncate -s $((PSECT * 512)) "$T/part.img"
mkfs.vfat -F 32 -s 1 -i 54534346 -n TSCONF --invariant "$T/part.img" >/dev/null
MTOOLS_SKIP_CHECK=1 mcopy -i "$T/part.img" -m -s "$T"/src/* ::
dd if="$T/part.img" of="$OUT" bs=512 seek=128 conv=notrunc status=none
printf 'label: dos\nlabel-id: 0x54534346\nunit: sectors\n\n128 %s c\n' "$PSECT" | sfdisk --quiet "$OUT" >/dev/null
echo "wrote $OUT ($MIB MiB, partition at sector 128, $PSECT sectors)"
