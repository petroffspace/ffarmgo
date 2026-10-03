#!/usr/bin/env python3
"""Map a PE32 file's sections to virtual addresses (no dependencies)."""
import struct


def load(path):
    d = open(path, 'rb').read()
    pe = struct.unpack_from('<I', d, 0x3c)[0]
    nsec = struct.unpack_from('<H', d, pe + 6)[0]
    optsz = struct.unpack_from('<H', d, pe + 20)[0]
    base = struct.unpack_from('<I', d, pe + 24 + 28)[0]
    secs = []
    off = pe + 24 + optsz
    for i in range(nsec):
        name = d[off:off + 8].rstrip(b'\0').decode()
        vsize, va, rawsz, rawptr = struct.unpack_from('<IIII', d, off + 8)
        secs.append((name, base + va, vsize, d[rawptr:rawptr + min(rawsz, vsize or rawsz)]))
        off += 40
    return base, secs
