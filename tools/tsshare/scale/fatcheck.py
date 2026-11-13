#!/usr/bin/python3 -I
"""Independent FAT32 reader and checker for tsshare perf runs (stdlib only).

  fatcheck.py list  VHD                       print "size<TAB>path" for every file
  fatcheck.py tree  VHD SRC_DIR  N SEED       md5 N random source files vs the VHD
  fatcheck.py zip   VHD SRC_ZIP  N SEED TOPN [all]  md5 N random + TOPN largest + all non-ASCII
                                              entries (+ every entry with 'all') vs the VHD
  fatcheck.py extents VHD OUT                 write "path size startoffset" lines (byte offset in VHD)

It follows the FAT chain for every read (no assumption of contiguity) and
verifies the LFN checksum of every long name.
"""
import hashlib, os, random, struct, sys, time, zipfile


class Fat:
    def __init__(self, path):
        self.f = open(path, 'rb', buffering=0)
        mbr = self.f.read(512)
        assert mbr[510:512] == b'\x55\xaa', 'MBR signature'
        self.pstart = struct.unpack_from('<I', mbr, 446 + 8)[0]
        b = self.pread(self.pstart * 512, 512)
        assert b[510:512] == b'\x55\xaa'
        (bps,) = struct.unpack_from('<H', b, 11)
        self.spc = b[13]
        rsv = struct.unpack_from('<H', b, 14)[0]
        nfat = b[16]
        self.fatsz = struct.unpack_from('<I', b, 36)[0]
        self.root = struct.unpack_from('<I', b, 44)[0]
        assert bps == 512 and nfat == 2
        self.cb = self.spc * 512
        self.fat0 = (self.pstart + rsv) * 512
        self.data0 = (self.pstart + rsv + nfat * self.fatsz) * 512
        raw = self.pread(self.fat0, self.fatsz * 512)
        self.fat = struct.unpack('<%dI' % (len(raw) // 4), raw)

    def pread(self, off, n):
        return os.pread(self.f.fileno(), n, off)

    def chain(self, c):
        out = []
        while 2 <= c < 0x0FFFFFF8:
            out.append(c)
            c = self.fat[c] & 0x0FFFFFFF
            assert len(out) < 1 << 24
        return out

    def coff(self, c):
        return self.data0 + (c - 2) * self.cb

    def read_chain(self, c, size):
        parts = []
        left = size
        # merge contiguous runs into single preads
        ch = self.chain(c) if size else []
        i = 0
        while i < len(ch) and left > 0:
            j = i
            while j + 1 < len(ch) and ch[j + 1] == ch[j] + 1:
                j += 1
            n = min((j - i + 1) * self.cb, left)
            parts.append(self.pread(self.coff(ch[i]), n))
            assert len(parts[-1]) == n
            left -= n
            i = j + 1
        return b''.join(parts)

    def iter_file(self, c, size, chunk=1 << 20):
        ch = self.chain(c) if size else []
        left = size
        i = 0
        while i < len(ch) and left > 0:
            j = i
            while j + 1 < len(ch) and ch[j + 1] == ch[j] + 1 and (j + 1 - i) * self.cb < chunk:
                j += 1
            n = min((j - i + 1) * self.cb, left)
            yield self.pread(self.coff(ch[i]), n)
            left -= n
            i = j + 1

    def walk(self):
        """yield (path, is_dir, size, start, date, time)"""
        stack = [('', self.root)]
        while stack:
            prefix, c = stack.pop()
            data = self.read_chain(c, len(self.chain(c)) * self.cb)
            lfn = []
            for off in range(0, len(data), 32):
                e = data[off:off + 32]
                if e[0] == 0:
                    break
                if e[0] == 0xE5:
                    lfn = []
                    continue
                attr = e[11]
                if attr == 0x0F:
                    lfn.append(e)
                    continue
                if attr & 0x08:
                    lfn = []
                    continue
                short = e[:11]
                if short[:1] == b'.':
                    lfn = []
                    continue
                if lfn:
                    exp = lfn[0][0] & 0x1F
                    assert lfn[0][0] & 0x40 and len(lfn) == exp, 'LFN sequence'
                    ck = 0
                    for ch_ in short:
                        ck = (((ck & 1) << 7) | (ck >> 1)) + ch_ & 0xFF
                    units = []
                    for k, l in enumerate(reversed(lfn)):
                        assert l[0] & 0x1F == k + 1 and l[13] == ck, 'LFN order/checksum'
                        units += list(struct.unpack_from('<5H', l, 1)) + list(struct.unpack_from('<6H', l, 14)) + list(struct.unpack_from('<2H', l, 28))
                    raw = b''.join(struct.pack('<H', u) for u in units)
                    name = raw.decode('utf-16-le').split('\x00')[0]
                else:
                    n = short[:8].decode('cp437').rstrip()
                    x = short[8:].decode('cp437').rstrip()
                    name = n + ('.' + x if x else '')
                lfn = []
                start = struct.unpack_from('<H', e, 20)[0] << 16 | struct.unpack_from('<H', e, 26)[0]
                size = struct.unpack_from('<I', e, 28)[0]
                mtime = struct.unpack_from('<H', e, 22)[0]
                mdate = struct.unpack_from('<H', e, 24)[0]
                isdir = bool(attr & 0x10)
                yield (prefix + name, isdir, size, start, mdate, mtime)
                if isdir:
                    stack.append((prefix + name + '/', start))


def decode_ts(d, t):
    return ((d >> 9) + 1980, (d >> 5) & 15, d & 31, t >> 11, (t >> 5) & 63, (t & 31) * 2)


def md5_chain(fat, start, size):
    h = hashlib.md5()
    for b in fat.iter_file(start, size):
        h.update(b)
    return h.hexdigest()


def md5_file(p, size=None):
    h = hashlib.md5()
    with open(p, 'rb') as f:
        while True:
            b = f.read(1 << 20)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def index(fat):
    files, dirs = {}, {}
    for p, isdir, size, start, d, t in fat.walk():
        (dirs if isdir else files)[p] = (size, start, d, t)
    return files, dirs


def lookup(files, p):
    if p in files:
        return p, files[p]
    return None, None


def pct(v, q):
    v = sorted(v)
    return v[min(len(v) - 1, int(q * len(v)))]


def summary(label, v):
    if not v:
        return '%-28s n=0' % label
    return '%-28s n=%-6d median %7d  p95 %7d  p99 %7d  max %8d  (us)' % (label, len(v), pct(v, .5), pct(v, .95), pct(v, .99), max(v))


def server_lats(logpath, ops):
    """join ops [(off, label, client_us)] with the daemon's -v log lines
    `ts off len kind lat_us` by offset (in order of appearance)"""
    by = {}
    with open(logpath) as f:
        for l in f:
            w = l.split()
            if len(w) == 5 and w[0].isdigit():
                by.setdefault(int(w[1]), []).append(int(w[4]))
    out = {}
    miss = 0
    for off, label, _c in ops:
        q = by.get(off)
        if not q:
            miss += 1
            continue
        out.setdefault(label, []).append(q.pop(0))
    return out, miss


def main():
    cmd = sys.argv[1]
    fat = Fat(sys.argv[2])
    if cmd == 'list':
        files, dirs = index(fat)
        for p, (size, *_r) in sorted(files.items()):
            print('%d\t%s' % (size, p))
        print('# %d files %d dirs' % (len(files), len(dirs)), file=sys.stderr)
    elif cmd == 'extents':
        files, dirs = index(fat)
        with open(sys.argv[3], 'w') as o:
            for p, (size, start, d, t) in sorted(files.items()):
                if size:
                    o.write('%d\t%d\t%s\n' % (fat.coff(start), size, p))
    elif cmd in ('seq', 'rand'):
        # latency probes; argv: seq VHD LOG TOPN SEED [PATHFILE] | rand VHD LOG READS POOL SEED
        files, dirs = index(fat)
        logp = sys.argv[3]
        fd = fat.f.fileno()
        def contiguous(start, size):
            ch = fat.chain(start)
            assert all(ch[i + 1] == ch[i] + 1 for i in range(len(ch) - 1)), 'fragmented'
            return fat.coff(start)
        ops = []
        if cmd == 'seq':
            topn, seed = int(sys.argv[4]), int(sys.argv[5])
            if len(sys.argv) > 6:  # optional file with one path per line to read instead of the largest
                names = [l.rstrip('\n') for l in open(sys.argv[6])]
                big = [(n, files[n]) for n in names]
            else:
                big = sorted(files.items(), key=lambda kv: -kv[1][0])[:topn]
            random.Random(seed).shuffle(big)
            print('file order:', [b[0].rsplit('/', 1)[-1][:30] + ' ' + str(b[1][0]) for b in big][:5], '...')
            for path, (size, start, d, t) in big:
                base = contiguous(start, size)
                for tag in ('cold', 'warm'):
                    t0 = time.perf_counter()
                    for off in range(0, size, 16384):
                        n = min(16384, size - off)
                        a = time.perf_counter()
                        b = os.pread(fd, n, base + off)
                        us = int((time.perf_counter() - a) * 1e6)
                        assert len(b) == n
                        ops.append((base + off, tag, us))
                    wall = time.perf_counter() - t0
                    print('%-4s %8d B %-40.40s whole-file wall %7.3f s (%d reads)' % (tag, size, path.rsplit('/', 1)[-1], wall, (size + 16383) // 16384))
        else:
            reads, pool, seed = int(sys.argv[4]), int(sys.argv[5]), int(sys.argv[6])
            rnd = random.Random(seed)
            cand = [(p, v) for p, v in files.items() if v[0] >= 16384]
            chosen = rnd.sample(cand, pool)
            touched = set()
            for i in range(reads):
                path, (size, start, d, t) = chosen[rnd.randrange(pool)]
                base = contiguous(start, size)
                off = rnd.randrange(0, (size - 16384) // 512 + 1) * 512
                tag = 'warm' if path in touched else 'cold'
                touched.add(path)
                a = time.perf_counter()
                b = os.pread(fd, 16384, base + off)
                us = int((time.perf_counter() - a) * 1e6)
                assert len(b) == 16384
                ops.append((base + off, tag, us))
        time.sleep(0.6)  # the daemon flushes its log every 100 ms
        srv, miss = server_lats(logp, ops)
        for tag in ('cold', 'warm'):
            print(summary('%s client' % tag, [c for _o, l, c in ops if l == tag]))
            print(summary('%s server (log)' % tag, srv.get(tag, [])))
        print('ops %d, not matched in log %d' % (len(ops), miss))
    elif cmd == 'tree':
        src, n, seed = sys.argv[3], int(sys.argv[4]), int(sys.argv[5])
        files, dirs = index(fat)
        allf = []
        for dp, dn, fn in os.walk(src):
            for f in fn:
                allf.append(os.path.join(dp, f))
        allf.sort()
        rnd = random.Random(seed)
        sample = rnd.sample(allf, n)
        ok = bad = ren = tsbad = 0
        for p in sample:
            rel = os.path.relpath(p, src)
            key, ent = lookup(files, rel)
            if ent is None:
                ren += 1
                print('RENAMED/NOT FOUND (exact path):', rel[:120])
                continue
            size, start, d, t = ent
            ss = os.stat(p)
            m = md5_file(p)
            m2 = md5_chain(fat, start, size)
            if ss.st_size == size and m == m2:
                ok += 1
            else:
                bad += 1
                print('MISMATCH', rel)
            lt = time.localtime(ss.st_mtime)
            exp = (min(max(lt.tm_year, 1980), 2107), lt.tm_mon, lt.tm_mday, lt.tm_hour, lt.tm_min, lt.tm_sec // 2 * 2)
            if lt.tm_year < 1980 or lt.tm_year > 2107:
                exp = None
            if exp and decode_ts(d, t) != exp:
                tsbad += 1
                print('MTIME', rel[:80], decode_ts(d, t), exp)
        print('tree: sampled %d  ok %d  mismatch %d  not-found(renamed) %d  mtime-diff %d  (FAT files %d, source files %d)' % (n, ok, bad, ren, tsbad, len(files), len(allf)))
        sys.exit(1 if bad else 0)
    elif cmd == 'zip':
        zp, n, seed, topn = sys.argv[3], int(sys.argv[4]), int(sys.argv[5]), int(sys.argv[6])
        files, dirs = index(fat)
        z = zipfile.ZipFile(zp)
        infos = [i for i in z.infolist() if not i.is_dir()]
        rnd = random.Random(seed)
        sample = rnd.sample(infos, n)
        top = sorted(infos, key=lambda i: -i.file_size)[:topn]
        ok = bad = nf = 0
        seen = set()
        groups = [('random', sample), ('top', top),
                  ('non-ASCII', [i for i in infos if not i.filename.isascii()])]
        if len(sys.argv) > 7 and sys.argv[7] == 'all':
            groups.append(('all', infos))
        for tag, group in groups:
            g_ok = g_bad = g_nf = 0
            for zi in group:
                key, ent = lookup(files, zi.filename)
                if ent is None:
                    g_nf += 1
                    print('NOT FOUND (exact path):', tag, repr(zi.filename[:140]))
                    continue
                size, start, d, t = ent
                h = hashlib.md5()
                with z.open(zi) as zf:
                    while True:
                        b = zf.read(1 << 20)
                        if not b:
                            break
                        h.update(b)
                if size == zi.file_size and h.hexdigest() == md5_chain(fat, start, size):
                    g_ok += 1
                else:
                    g_bad += 1
                    print('MISMATCH', tag, repr(zi.filename))
            print('zip %s: %d checked, ok %d, mismatch %d, not-found %d' % (tag, len(group), g_ok, g_bad, g_nf))
            bad += g_bad
        sys.exit(1 if bad else 0)


main()
