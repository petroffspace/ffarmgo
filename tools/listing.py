#!/usr/bin/env python3
"""Query the Ghidra listing export (ffarm.decompiled) of ffarm.8bf.

The export is disassembly plus data bytes (no C pseudocode). This tool
rebuilds the memory image and an instruction index from it, caches both
next to the listing, and answers the questions RE work keeps asking:

  listing.py dis ADDR [ADDR...]   clean disassembly of the function(s) containing ADDR
  listing.py range LO HI          disassembly of an arbitrary address range (for LAB_ bodies)
  listing.py mem ADDR [LEN]       hex dump of the image (default 64 bytes)
  listing.py f64 ADDR [COUNT]     little-endian doubles (bits + value)
  listing.py f32 ADDR [COUNT]     little-endian floats
  listing.py str ADDR             NUL-terminated string
  listing.py xref NAME            instructions mentioning NAME (e.g. FUN_10005850, DAT_1000eff8)
  listing.py callers ADDR         functions that CALL/JMP the function at ADDR
  listing.py funcs                every FUNCTION banner with its size in bytes
  listing.py ops                  operator registry: slot, name, result/child types, ctor
  listing.py types                runtime type table registered via FUN_10008030

Addresses are hex, with or without 0x / the 1000 prefix (a2e8 == 1000a2e8).
Uninitialized data reads as zero (or absent), so values the plugin fills
in at startup (e.g. DAT_1000e8f8, written by FUN_100088d0) must be
recovered from the code that writes them.
"""
import bisect
import json
import os
import re
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, '..', 'ffarm.decompiled')
CACHE = os.path.join(HERE, '..', '.listing-cache.json')
IMAGE_LO, IMAGE_HI = 0x10000000, 0x10010000
TEXT_LO, TEXT_HI = 0x10001000, 0x10009000

HEAD = re.compile(r'^ {8}([0-9a-f]{8}) ((?:[0-9a-f]{2} ?)+)')
CONT = re.compile(r'^ {17}((?:[0-9a-f]{2} ?)+)\s*$')
LABEL = re.compile(r'^ {29}([A-Za-z_][\w:.@?$]*)(?:\s+XREF.*)?$')


def parse():
    mem, ins, labels, funcs = {}, [], {}, []
    pending_label, pending_func, cur = None, False, None
    with open(SRC, encoding='utf-8', errors='replace') as fh:
        for line in fh:
            line = line.rstrip('\n')
            if '*                          FUNCTION' in line:
                pending_func = True
                continue
            m = LABEL.match(line)
            if m:
                pending_label = m.group(1)
                continue
            m = HEAD.match(line)
            if m:
                a = int(m.group(1), 16)
                if not IMAGE_LO <= a < IMAGE_HI:
                    cur = None
                    continue
                cur = a
                for tok in line[17:26].split():  # bytes column (first 3 bytes)
                    mem[cur] = int(tok, 16)
                    cur += 1
                if pending_label:
                    labels[a] = pending_label
                    pending_label = None
                if pending_func:
                    funcs.append(a)
                    pending_func = False
                mn = line[33:44].strip()
                rest = line[44:].strip()
                parts = re.split(r'\s{2,}', rest, maxsplit=1) if rest else ['']
                txt = (mn + ' ' + parts[0]).strip()
                if len(parts) > 1 and parts[1].startswith('='):
                    txt += '    ; ' + re.sub(r' {2,}', ' ', parts[1])
                ins.append((a, txt))
                continue
            m = CONT.match(line)
            if m and cur is not None:
                for tok in m.group(1).split():
                    mem[cur] = int(tok, 16)
                    cur += 1
    return {'mem': {str(k): v for k, v in mem.items()}, 'ins': ins,
            'labels': {str(k): v for k, v in labels.items()}, 'funcs': sorted(funcs)}


def load():
    if not os.path.exists(SRC):
        sys.exit('missing %s: export a Ghidra listing of ffarm.8bf there '
                 '(it is not part of the ffarmgo release)' % os.path.normpath(SRC))
    if os.path.exists(CACHE) and os.path.getmtime(CACHE) >= os.path.getmtime(SRC):
        with open(CACHE) as fh:
            d = json.load(fh)
    else:
        d = parse()
        with open(CACHE, 'w') as fh:
            json.dump(d, fh)
    d['mem'] = {int(k): v for k, v in d['mem'].items()}
    d['labels'] = {int(k): v for k, v in d['labels'].items()}
    return d


def addr(s):
    s = s.lower().removeprefix('0x').removeprefix('fun_').removeprefix('dat_').removeprefix('lab_')
    a = int(s, 16)
    return a if a >= IMAGE_LO else IMAGE_LO + a


def read(d, a, n):
    out = []
    for i in range(n):
        if a + i not in d['mem']:
            return None
        out.append(d['mem'][a + i])
    return bytes(out)


def func_range(d, a):
    f = d['funcs']
    i = bisect.bisect_right(f, a) - 1
    lo = f[i] if i >= 0 else a
    hi = f[i + 1] if i + 1 < len(f) else lo + 0x400
    return lo, min(hi, TEXT_HI) if lo < TEXT_HI else hi


def cmd_dis(d, args):
    for s in args:
        lo, hi = func_range(d, addr(s))
        print(f'===== {lo:08x}-{hi:08x}')
        for a, t in d['ins']:
            if lo <= a < hi and not t.startswith('??'):
                if a in d['labels']:
                    print(f"{d['labels'][a]}:")
                print(f'  {a:08x}  {t}')


def cmd_range(d, args):
    lo, hi = addr(args[0]), addr(args[1])
    for a, t in d['ins']:
        if lo <= a < hi and not t.startswith('??'):
            if a in d['labels']:
                print(f"{d['labels'][a]}:")
            print(f'  {a:08x}  {t}')


def cmd_mem(d, args):
    a = addr(args[0])
    n = int(args[1], 0) if len(args) > 1 else 64
    for row in range(a, a + n, 16):
        cells = [f"{d['mem'][x]:02x}" if x in d['mem'] else '..' for x in range(row, min(row + 16, a + n))]
        print(f'{row:08x}  {" ".join(cells)}')


def cmd_num(d, args, size, fmt, ifmt):
    a = addr(args[0])
    count = int(args[1], 0) if len(args) > 1 else 1
    for i in range(count):
        b = read(d, a + i * size, size)
        if b is None:
            print(f'{a + i * size:08x}  (not initialized in the image)')
            continue
        bits = struct.unpack(ifmt, b)[0]
        print(f'{a + i * size:08x}  {bits:0{size * 2}X}  {struct.unpack(fmt, b)[0]!r}')


def cmd_str(d, args):
    a, out = addr(args[0]), []
    while a in d['mem'] and d['mem'][a] != 0:
        out.append(d['mem'][a])
        a += 1
    print(bytes(out).decode('latin-1'))


def cmd_xref(d, args):
    pat = re.compile(re.escape(args[0]), re.I)
    for a, t in d['ins']:
        if pat.search(t):
            lo, _ = func_range(d, a)
            print(f'{a:08x}  [{lo:08x}]  {t}')


def cmd_callers(d, args):
    target = f'{addr(args[0]):08x}'
    seen = set()
    for a, t in d['ins']:
        if re.match(rf'(CALL|JMP) (FUN_|LAB_){target}\b', t):
            seen.add(func_range(d, a)[0])
    for f in sorted(seen):
        print(f'{f:08x}')


def cmd_funcs(d, args):
    f = [x for x in d['funcs'] if x < TEXT_HI]
    for i, a in enumerate(f):
        hi = f[i + 1] if i + 1 < len(f) else TEXT_HI
        print(f'{a:08x}  {hi - a:5d}  {d["labels"].get(a, "")}')


def type_names(d):
    """Map DAT_ slot -> runtime type ID by replaying FUN_10008030 calls in
    init order (the counter pre-increments, so IDs start at 1)."""
    order = [0x10005430, 0x10007600, 0x10008c90, 0x10008a70, 0x100045e0]
    ids, n = {}, 0
    for f in order:
        lo, hi = func_range(d, f)
        last_call = False
        for a, t in d['ins']:
            if not lo <= a < hi:
                continue
            if t.startswith('CALL FUN_10008030'):
                n += 1
                last_call = True
            m = re.match(r'MOV (?:dword ptr )?\[?(DAT_[0-9a-f]+)\]?,EAX', t)
            if m and last_call:
                ids[m.group(1)] = n
                last_call = False
    return ids


def cmd_types(d, args):
    for slot, tid in sorted(type_names(d).items(), key=lambda kv: kv[1]):
        print(f'{tid}  {slot}')


def cmd_ops(d, args):
    """Replay the registrars in init order, tracking pushes symbolically."""
    tids = type_names(d)
    order = [0x10007720, 0x10008b80, 0x10008260, 0x10006f70, 0x10008cd0, 0x100046b0, 0x100080d0]
    slot = 0
    for f in order:
        lo, _ = func_range(d, f)
        regs, stack, spec = {}, [], None
        for a, t in d['ins']:
            if a < lo:
                continue
            if t.startswith('RET'):
                break
            m = re.match(r'MOV (E[A-Z]X),(?:dword ptr )?\[?(DAT_[0-9a-f]+)\]?$', t)
            if m:
                regs[m.group(1)] = str(tids.get(m.group(2), m.group(2)))
                continue
            m = re.match(r'PUSH (\S+)', t)
            if m:
                v = m.group(1)
                if v in regs:
                    v = regs[v]
                elif v.startswith(('s_', 'DAT_1000a')):
                    sa = addr(re.search(r'1000[0-9a-f]{4}$', v).group(0))
                    out = []
                    while sa in d['mem'] and d['mem'][sa]:
                        out.append(d['mem'][sa])
                        sa += 1
                    v = '"' + bytes(out).decode('latin-1') + '"'
                elif v.startswith('0x'):
                    v = str(int(v, 16))
                stack.append(v)
                continue
            if t.startswith('CALL FUN_100054e0'):
                spec = stack[-4:][::-1]  # param_1..param_4
                del stack[-4:]
                continue
            if t.startswith('CALL FUN_10005500'):
                name, ctor = stack[-3], stack[-2]  # stack[-1] is the spec (PUSH EAX)
                del stack[-3:]
                ret, kids = spec[0], [k for k in spec[1:]]
                arity = next((i for i, k in enumerate(kids) if k == '0'), 3)
                print(f'{slot:2d}  {name:12s} ret={ret:2s} children={kids[:arity]}  ctor={ctor}')
                slot += 1


def main():
    if len(sys.argv) < 2 or sys.argv[1] in ('-h', '--help'):
        print(__doc__)
        return
    d = load()
    cmd, args = sys.argv[1], sys.argv[2:]
    table = {'dis': cmd_dis, 'range': cmd_range, 'mem': cmd_mem, 'str': cmd_str, 'xref': cmd_xref,
             'callers': cmd_callers, 'funcs': cmd_funcs, 'ops': cmd_ops, 'types': cmd_types,
             'f64': lambda d, a: cmd_num(d, a, 8, '<d', '<Q'),
             'f32': lambda d, a: cmd_num(d, a, 4, '<f', '<I')}
    if cmd not in table:
        sys.exit(f'unknown command {cmd!r}; see --help')
    table[cmd](d, args)


if __name__ == '__main__':
    main()
