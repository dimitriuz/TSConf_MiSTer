# fuse-vhd (spike, throwaway)

Read-only FUSE daemon exposing one file, `share.vhd` (mode 0444, default 4 GiB).
Content is the backing image where it exists, zeros beyond (logged `synthetic`).
Writes return EROFS.

## Build
    go build -o fuse-vhd .
    GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o fuse-vhd-arm .
go-fuse is pinned to v2.11.0: v2.12.x does not compile for 32-bit ARM (splice package).

## Test image
    ./mkimage.sh /tmp/test.img 256     # MBR + FAT32 at sector 128, 512-byte clusters, reproducible

## Run
    fuse-vhd -backing test.img -mount /mnt/vhd [-size 4294967296] [-log reads.log] [-delay 50ms]
Log line: `ts_ns offset len backing|synthetic lat_us`.
The mount uses `DirectMount: true`. As root it calls mount(2) directly (target board, no fusermount);
as a normal user go-fuse falls back to `fusermount3` automatically (local testing).

## Abort
SIGINT/SIGTERM unmounts and exits cleanly. If the daemon is dead and the mount is stuck:
`fusermount3 -u <dir>` (local) or `umount -l <dir>` (root, board). `kill -9` leaves a dead mount: unmount it as above.

## Notes
`fsck.vfat` here (4.2) has no `--offset`; check the partition by extracting it through the mount:
`dd if=<mnt>/share.vhd of=part.img bs=512 skip=128 count=<sectors>; fsck.vfat -n part.img`.
`mdir -i <mnt>/share.vhd@@65536 ::` works directly.
