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
SIGINT/SIGTERM unmounts and exits cleanly. If the unmount fails because the mount is busy
(log shows `unmount: ... busy`), the daemon tries a lazy unmount itself; you can do it by hand:
`umount -l <dir>` (root, board) or `fusermount3 -uz <dir>` (local); the daemon then exits by itself.
A second SIGINT/SIGTERM makes it attempt the lazy unmount and exit immediately.
`kill -9` leaves a dead mount ("Transport endpoint is not connected"), which also needs `umount -l <dir>`.
If the daemon hangs, abort the connection through fusectl (root): find the mount's connection number N in
`/sys/fs/fuse/connections/`, then `echo 1 > /sys/fs/fuse/connections/<N>/abort` (mount fusectl first with
`mount -t fusectl none /sys/fs/fuse/connections` if the directory is empty).

The read log is written by a background goroutine (flush every 100 ms and at exit, file truncated at start);
lines are dropped, never blocked on, if the queue fills; the drop count is printed on exit. A backing read
error (not EOF) returns EIO and is logged as `error`.

## Notes
`fsck.vfat` here (4.2) has no `--offset`; check the partition by extracting it through the mount:
`dd if=<mnt>/share.vhd of=part.img bs=512 skip=128 count=<sectors>; fsck.vfat -n part.img`.
`mdir -i <mnt>/share.vhd@@65536 ::` works directly.
