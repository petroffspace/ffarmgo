#!/usr/bin/env python3
"""Generate emulator-backed test vectors for internal/farm.

Each vector is a random but type-correct .gdo filter, a random source
frame (RGB, RGB+alpha, gray, gray+alpha or CMYK), an optional selection
mask, and the output of the ORIGINAL plugin's final render
(FUN_10001240) run under tools/emu.py. The Go test
TestEmulatorVectors renders the same inputs and must match byte for
byte.

    python3 tools/gen_vectors.py [-n COUNT] [-seed S] [-out DIR]

Before writing anything it re-checks the emulator against the real
Photoshop render real_plugin/ffarm1.png on a cropped region.
"""
import argparse
import base64
import json
import math
import os
import random
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from emu import Plugin  # noqa: E402

S, B, P, M, I = 2, 4, 8, 7, 9  # runtime type IDs
OPS = {  # slot: (result type, child types) — tools/listing.py ops
    0: (S, []), 1: (S, []), 2: (S, [S]), 3: (S, [S, S]), 4: (S, [S, S]),
    5: (S, [S, S]), 6: (S, [S, S, S]), 7: (S, [S, S, S]), 8: (S, [S, S]),
    9: (S, [S, S]), 10: (M, [P, S]), 11: (S, [S]), 12: (B, [S]), 13: (P, [S]),
    14: (S, [S]), 15: (P, [S]), 16: (S, [S, S]), 17: (I, [S, S, S]),
    18: (I, [S, I, I]), 19: (I, [S, I, I]), 20: (I, [I, M]), 21: (S, [S, M]),
}
NAMES = ['mu', 'mv', 'minpe', 'peplus', 'petimes', 'pepow', 'switch', 'mix', 'bias', 'gain',
         'ri2cx', 'dopnoise', 'dopbnoise', 'dopdnoise', 'rfract', 'bfract', 'dither',
         'makrgb', 'gswitch', 'gmix', 'trfmap', 'trfmap']



def require_plugin(path):
    """The original ffarm.8bf is not distributed with ffarmgo; put a copy in
    real_plugin/ (with ffarm1.png for the reference check) to use this tool."""
    if not os.path.exists(path):
        sys.exit('missing %s: copy the original Filter Farm plugin there '
                 '(it is not part of the ffarmgo release)' % path)
    return path

def candidates(t):
    if t == S:  # dopbnoise's bipolar double is scalar-compatible
        return [o for o, (r, _) in OPS.items() if r in (S, B)]
    return [o for o, (r, _) in OPS.items() if r == t]


def pool(rng, t, focus):
    """A weighted list for a slot requesting type t."""
    cands = candidates(t)
    k = min(len(cands), rng.choice([1, 2, 3, 4]))
    picks = rng.sample(cands, k)
    if focus in cands and focus not in picks and rng.random() < 0.6:
        picks[0] = focus
    # plasma ctors are expensive to emulate: keep them rarer
    weights = [rng.randint(1, 0xffff) // (6 if o in (14, 15) else 1) for o in picks]
    total = sum(weights)
    scale = rng.choice([1.0, 1.0, 0.9, 0.5])  # < 1 exercises the selector fallback
    return [(o, max(0, min(0xffff, int(w / total * 65535 * scale)))) for o, w in zip(picks, weights)]


def write_gdo(rng, root_type, focus):
    out = bytearray(struct.pack('<HH', 0x03E8, root_type))
    seed = rng.getrandbits(48)
    out += struct.pack('<II4sHHH2s', 0, 0, bytes(4), seed & 0xffff, (seed >> 16) & 0xffff, seed >> 32, bytes(2))

    def lst(entries):
        b = struct.pack('<H', len(entries))
        for o, w in entries:
            b += struct.pack('<HH', o, w)
        return b
    out += lst(pool(rng, root_type, focus))
    out += struct.pack('<H', len(OPS))
    for op in range(len(OPS)):
        kids = OPS[op][1]
        for pos in range(3):
            for depth in range(4):
                out += lst(pool(rng, kids[pos], focus) if pos < len(kids) else [])
    out += struct.pack('<H', 0)
    return bytes(out)


def frame(rng, w, h, planes, channels):
    params = [(rng.uniform(0.02, 0.3), rng.uniform(0.02, 0.3), rng.uniform(0, 6.3)) for _ in range(planes)]
    pix = bytearray()
    for y in range(h):
        for x in range(w):
            for c in range(planes):
                a, b, ph = params[c]
                v = 127.5 + 110 * math.sin(a * x + b * y + ph) + rng.uniform(-15, 15)
                pix.append(max(0, min(255, int(v))))
    return bytes(pix)


def mask(rng, w, h):
    kind = rng.choice(['none', 'none', 'soft', 'rect'])
    if kind == 'none':
        return None
    m = bytearray(w * h)
    if kind == 'soft':
        for i in range(w * h):
            m[i] = rng.choice([0, 255, rng.randint(1, 254)])
    else:
        x0, x1 = sorted(rng.sample(range(w + 1), 2))
        y0, y1 = sorted(rng.sample(range(h + 1), 2))
        for y in range(y0, y1):
            for x in range(x0, x1):
                m[y * w + x] = 255 if rng.random() < 0.9 else 0
    return bytes(m)


def check_emulator(plugin_path, root):
    """Emulated plugin vs the real Photoshop render, on a 640×360 white
    frame (the reference covers the whole image; ~2 minutes)."""
    sys.path.insert(0, root)
    import zlib
    ref = open(os.path.join(root, 'real_plugin', 'ffarm1.png'), 'rb').read()
    p = Plugin(plugin_path)
    gdo = open(os.path.join(root, 'internal', 'farm', 'testdata', 'render.gdo'), 'rb').read()
    out = p.render(gdo, 640, 360, 3, 3, b'\xff' * (640 * 360 * 3))
    # decode the reference PNG (8-bit RGB, filters 0-4)
    i, idat = 8, b''
    while i < len(ref):
        n, t = struct.unpack('>I4s', ref[i:i + 8])
        if t == b'IDAT':
            idat += ref[i + 8:i + 8 + n]
        i += 12 + n
    raw, stride, prev, pix, pos = zlib.decompress(idat), 640 * 3, bytearray(640 * 3), bytearray(), 0
    for _ in range(360):
        f, line = raw[pos], bytearray(raw[pos + 1:pos + 1 + stride])
        pos += 1 + stride
        for x in range(stride):
            a = line[x - 3] if x >= 3 else 0
            b, c = prev[x], (prev[x - 3] if x >= 3 else 0)
            pr = [0, a, b, (a + b) // 2, a if abs(b - c) <= abs(a - c) and abs(b - c) <= abs(a + b - 2 * c)
                  else (b if abs(a - c) <= abs(a + b - 2 * c) else c)][f]
            line[x] = (line[x] + pr) & 255
        pix += line
        prev = line
    if out != bytes(pix):
        sys.exit('emulator does NOT reproduce real_plugin/ffarm1.png — refusing to write vectors')
    print('emulator reproduces the real Photoshop render exactly')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('-n', type=int, default=48)
    ap.add_argument('-seed', type=int, default=1)
    ap.add_argument('-out', default=None)
    ap.add_argument('-skip-check', action='store_true')
    ap.add_argument('-strips', action='store_true',
                    help='small maxSpace values so the plugin renders in several strips (names s###)')
    a = ap.parse_args()
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    plugin = require_plugin(os.path.join(root, 'real_plugin', 'ffarm.8bf'))
    out = a.out or os.path.join(root, 'internal', 'farm', 'testdata', 'emu')
    if not a.skip_check:
        check_emulator(plugin, root)
    os.makedirs(out, exist_ok=True)
    rng = random.Random(a.seed)
    poisons = (bytes.fromhex('0000F87F4C3A2E10'), bytes.fromhex('9A99999999990540'))  # NaN-ish, 2.7
    modes = [(3, 3, 'rgb'), (3, 3, 'rgb'), (4, 3, 'rgba'), (1, 1, 'gray'), (2, 1, 'graya'), (4, 4, 'cmyk')]
    n = attempts = 0
    while n < a.n:
        attempts += 1
        focus = n % len(OPS)  # rotate a favoured operator through the registry
        planes, channels, mode = rng.choice(modes)
        w, h = rng.choice([(40, 30), (33, 21), (64, 16), (24, 40)])
        # In three-channel modes the plugin reads an image value; with a
        # scalar root it would read G and B from an uninitialized stack
        # buffer (host-dependent), so RGB vectors always use image roots.
        root_type = I if channels == 3 else rng.choice([I, S])
        gdo = write_gdo(rng, root_type, focus)
        src = frame(rng, w, h, planes, channels)
        m = mask(rng, w, h)
        max_space = 0x7fffffff
        if a.strips:  # rows per strip between 1 and h−1: maxSpace ≈ rows·(2·planes+1)·width
            max_space = rng.randint(1, h - 1) * (2 * planes + 1) * w - rng.randint(0, 3)
        t = time.time()
        res = Plugin(plugin).render(gdo, w, h, planes, channels, src, m, max_space)  # fresh process per vector
        # The original reads uninitialized memory on some trees (e.g. a
        # pixel-reading leaf under a coordinate-only plasma child sees the
        # plasma ctor's unset ix/iy). Such output is host-dependent: skip.
        if any(Plugin(plugin, poison=pz).render(gdo, w, h, planes, channels, src, m, max_space) != res for pz in poisons):
            print('skipped: output depends on uninitialized memory', flush=True)
            continue
        vec = dict(name='%s%03d_%s_%s%s' % ('s' if a.strips else 'v', n, NAMES[focus], mode, '_mask' if m else ''),
                   max_space=max_space if a.strips else None,
                   w=w, h=h, planes=planes, channels=channels,
                   gdo=base64.b64encode(gdo).decode(), src=base64.b64encode(src).decode(),
                   mask=base64.b64encode(m).decode() if m else None, out=base64.b64encode(res).decode())
        with open(os.path.join(out, vec['name'] + '.json'), 'w') as fh:
            json.dump(vec, fh)
        print('%-28s %5.1fs' % (vec['name'], time.time() - t), flush=True)
        n += 1
    print('%d vectors written, %d candidates skipped as undefined' % (n, attempts - n))


if __name__ == '__main__':
    main()
