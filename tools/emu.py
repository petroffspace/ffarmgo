#!/usr/bin/env python3
"""Run ffarm.8bf's own machine code under Unicorn (pip install unicorn).

The plugin is mapped at its preferred base and initialized with its own
init chain (FUN_100075b0). Its MSVCRT imports are replaced:

  calloc, malloc, free  size-bucketed allocator (freed blocks are reused)
  strncpy, qsort        Python implementations
  fread, fwrite, ftell, fseek  in-memory files, by FILE handle (READ_FILE
                        for the readers, WRITE_FILE for the writers)
  time                  a fixed value (self.now), for new-session seeds
  atof                  Python float() of the string
  _ftol, floor          x87 machine-code stubs (truncate / round down)
  _CIpow                stub that pops ST1^ST0 and pushes the correctly
                        rounded x^y (exact decimal arithmetic) — the model
                        of MSVCRT's x87 pow, matching internal/x87.Pow

The FPU starts with the Windows default control word 0x27F (53-bit
precision, round to nearest).

Unicorn's x87 core (old QEMU) computes FYL2X and F2XM1 through host
doubles, which real hardware does not. The plugin's four such
instructions are therefore intercepted and computed as real x87 does,
modelled as correctly rounded to the 64-bit mantissa (internal/x87
ext.go uses the same model).

Plugin.render() drives the plugin's final-render routine FUN_10001240
through a fake Photoshop FilterRecord, so tree growth, every operator,
selection masks and image modes all run as the real plugin runs them.
"""
import math
import os
import struct
import sys

from unicorn import Uc, UC_ARCH_X86, UC_MODE_32, UC_HOOK_CODE, UcError
from unicorn.x86_const import (UC_X86_REG_ESP, UC_X86_REG_EAX, UC_X86_REG_EIP,
                               UC_X86_REG_FPSW, UC_X86_REG_FPTAG, UC_X86_REG_FP0)
from decimal import Decimal, getcontext

getcontext().prec = 60

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pe  # noqa: E402

STUBS, SCRATCH = 0x20000000, 0x2000A000
RET = 0x2000F000
STACK = 0x30000000
HEAP, HEAP_SIZE = 0x40000000, 0x08000000
HOST = 0x50000000  # filter record, stubs, image buffers

FTOL = bytes.fromhex('83EC0C D93C24 668B0424 660D000C 6689442402 D96C2402 DF7C2404 D92C24 8B442404 8B542408 83C40C C3'.replace(' ', ''))
FLOOR = bytes.fromhex('DD442404 83EC04 D93C24 668B0424 6625FFF3 660D0004 6689442402 D96C2402 D9FC D92C24 83C404 C3'.replace(' ', ''))
# fstp qword [Y]; fstp qword [X]; nop (Python computes R); fld qword [R]; ret
CIPOW = (b'\xdd\x1d' + struct.pack('<I', SCRATCH + 8) + b'\xdd\x1d' + struct.pack('<I', SCRATCH) +
         b'\x90' + b'\xdd\x05' + struct.pack('<I', SCRATCH + 16) + b'\xc3')
CIPOW_NOP = STUBS + 0x200 + 12
# atof: nop (Python parses the string into SCRATCH+24); fld qword [SCRATCH+24]; ret
ATOF = b'\x90' + b'\xdd\x05' + struct.pack('<I', SCRATCH + 24) + b'\xc3'
FPU_INIT = bytes.fromhex('687F020000D92C2483C404C3')  # push 0x27f; fldcw [esp]; add esp,4; ret

# Import thunks (JMP [IAT]) in the listing.
THUNKS = {
    'calloc': 0x10003fbc, 'free': 0x10003fc2, '_ftol': 0x10003fc8,
    'floor': 0x10008e90, '_CIpow': 0x10008e96, 'strncpy': 0x10008e9c,
    'exit': 0x10008ea2, 'printf': 0x10008ea8, 'qsort': 0x10008eae,
    'ftell': 0x10008ec0, 'fread': 0x10008ec6, 'malloc': 0x10008ecc, 'fseek': 0x10008ed2,
    'fwrite': 0x10008eb4, 'time': 0x10003fda, 'atof': 0x10008eba,
}

# x87 transcendental instructions in the plugin (tools/listing.py xref).
FYL2X_AT = (0x100089a4, 0x1000891c, 0x1000770e)
F2XM1_AT = (0x100089e2,)
LN2 = Decimal(2).ln()


def ext_to_dec(t):
    """Unicorn x87 register tuple (mantissa, sign|exponent) → Decimal."""
    man, se = t
    e = se & 0x7fff
    if man == 0 and e == 0:
        return Decimal(0)
    v = Decimal(man) * Decimal(2) ** (e - 16383 - 63)
    return -v if se & 0x8000 else v


def dec_to_ext(v):
    """Decimal → 80-bit register tuple, rounded to nearest-even."""
    if v == 0:
        return (0, 0)
    sign = 0x8000 if v < 0 else 0
    v = abs(v)
    k = 63 - int((v.ln() / LN2).to_integral_value(rounding='ROUND_FLOOR'))
    while v * Decimal(2) ** k < Decimal(2) ** 63:
        k += 1
    while v * Decimal(2) ** k >= Decimal(2) ** 64:
        k -= 1
    q = int((v * Decimal(2) ** k).to_integral_value(rounding='ROUND_HALF_EVEN'))
    if q == 1 << 64:
        q, k = 1 << 63, k - 1
    return (q, (63 - k + 16383) | sign)


def exact_pow(x, y):
    """Correctly rounded x^y with the special cases of internal/x87.Pow."""
    if y == 0 or x == 1:
        return 1.0
    if math.isnan(x) or math.isnan(y) or math.isinf(x) or math.isinf(y) or x == 0:
        try:
            return math.pow(x, y)
        except (OverflowError, ValueError):
            return float('inf') if x == 0 else float('nan')
    sign = 1.0
    if x < 0:
        if y != int(y):
            return float('nan')
        sign = -1.0 if int(y) % 2 else 1.0
        x = -x
    t = Decimal(x).ln() * Decimal(y)
    if abs(t) > 700:
        try:
            return sign * math.pow(x, y)
        except OverflowError:
            return sign * float('inf')
    return sign * float(t.exp())


READ_FILE, WRITE_FILE = 0xF11E, 0xF12E

DAT = dict(
    filter_record=0x1000efc8, record_index=0x1000aa34, records=0x1000a688,
    root_type=0x1000a3ac, channels=0x1000aba0, type_image=0x1000f0cc, type_scalar=0x1000eff8,
)


class Plugin:
    """poison: bytes repeated over the stack (before the render), into
    malloc'd blocks, and over the plasma constructor's context (the
    40-byte stack struct FUN_100065b0 receives as param_2, whose u/v/ix/iy
    the plasma sets only when its child's flags ask for them). Rendering
    with two different poisons exposes any dependence on uninitialized
    memory, including stale values the plugin itself left on its stack."""

    def __init__(self, path, poison=b'\0'):
        self.poison = poison
        self.base, secs = pe.load(path)
        uc = self.uc = Uc(UC_ARCH_X86, UC_MODE_32)
        uc.mem_map(self.base, 0x20000)
        for _, va, _, data in secs:
            uc.mem_write(va, data)
        uc.mem_map(STUBS, 0x10000)
        uc.mem_map(STACK - 0x100000, 0x100000)
        uc.mem_map(HEAP, HEAP_SIZE)
        uc.mem_map(HOST, 0x04000000)
        self.brk = HEAP
        self.files = {}   # FILE handle -> [bytearray, position]
        self.now = 0
        self.sizes = {}   # block address -> bucket size
        self.freed = {}   # bucket size -> [addresses]
        self.file = b''
        self.fpos = 0
        self.pow_calls = 0
        self.ext_cache = {}
        uc.mem_write(STUBS + 0x000, FTOL)
        uc.mem_write(STUBS + 0x100, FLOOR)
        uc.mem_write(STUBS + 0x200, CIPOW)
        uc.mem_write(STUBS + 0x300, FPU_INIT)
        uc.mem_write(RET, b'\xf4')
        self.jmp(THUNKS['_ftol'], STUBS + 0x000)
        self.jmp(THUNKS['floor'], STUBS + 0x100)
        self.jmp(THUNKS['_CIpow'], STUBS + 0x200)
        uc.mem_write(STUBS + 0x400, ATOF)
        self.jmp(THUNKS['atof'], STUBS + 0x400)
        uc.hook_add(UC_HOOK_CODE, self._atof, begin=STUBS + 0x400, end=STUBS + 0x400)
        uc.hook_add(UC_HOOK_CODE, self._pow, begin=CIPOW_NOP, end=CIPOW_NOP)
        for a in FYL2X_AT:
            uc.hook_add(UC_HOOK_CODE, self._fyl2x, begin=a, end=a)
        if poison != b'\0':
            uc.hook_add(UC_HOOK_CODE, self._poison_plasma_ctx, begin=0x100065b0, end=0x100065b0)
        for a in F2XM1_AT:
            uc.hook_add(UC_HOOK_CODE, self._f2xm1, begin=a, end=a)
        for name in ('calloc', 'malloc', 'free', 'strncpy', 'qsort', 'fread', 'ftell', 'fseek',
                     'fwrite', 'time', 'printf', 'exit'):
            uc.hook_add(UC_HOOK_CODE, self._import, user_data=name,
                        begin=THUNKS[name], end=THUNKS[name])
        self.call(STUBS + 0x300)
        self.call(0x100075b0)  # plugin init: types, registry, noise table, shaper

    # --- plumbing -----------------------------------------------------------

    def jmp(self, at, to):
        self.uc.mem_write(at, b'\xe9' + struct.pack('<i', to - (at + 5)))

    def call(self, addr, *args):
        esp = STACK - 0x1000
        frame = struct.pack('<I', RET) + b''.join(struct.pack('<I', a & 0xffffffff) for a in args)
        esp -= len(frame)
        self.uc.mem_write(esp, frame)
        self.uc.reg_write(UC_X86_REG_ESP, esp)
        self.uc.emu_start(addr, RET)
        return self.uc.reg_read(UC_X86_REG_EAX)

    def alloc(self, n, zero=True):
        size = max(16, (n + 15) & ~15)
        bucket = self.freed.get(size)
        if bucket:
            p = bucket.pop()
        else:
            p = self.brk
            self.brk += size
            if self.brk > HEAP + HEAP_SIZE:
                raise MemoryError('emulated heap exhausted')
        self.sizes[p] = size
        if zero:
            self.uc.mem_write(p, bytes(size))
        return p

    def release(self, p):
        size = self.sizes.pop(p, None)
        if size is not None:
            self.freed.setdefault(size, []).append(p)

    def u32(self, addr, n=1):
        return list(struct.unpack('<%dI' % n, self.uc.mem_read(addr, 4 * n)))

    def put(self, addr, fmt, *v):
        self.uc.mem_write(addr, struct.pack(fmt, *v))

    def _atof(self, uc, addr, size, _):
        p = struct.unpack('<I', uc.mem_read(uc.reg_read(UC_X86_REG_ESP) + 4, 4))[0]
        text = bytes(uc.mem_read(p, 64)).split(b'\0')[0].decode('latin-1')
        uc.mem_write(SCRATCH + 24, struct.pack('<d', float(text)))

    def _pow(self, uc, addr, size, _):
        x, y = struct.unpack('<dd', uc.mem_read(SCRATCH, 16))
        key = ('pow', x, y)
        r = self.ext_cache.get(key)
        if r is None:
            r = self.ext_cache[key] = exact_pow(x, y)
        uc.mem_write(SCRATCH + 16, struct.pack('<d', r))
        self.pow_calls += 1

    def _poison_plasma_ctx(self, uc, addr, size, _):
        ctx2 = struct.unpack('<I', uc.mem_read(uc.reg_read(UC_X86_REG_ESP) + 8, 4))[0]
        uc.mem_write(ctx2, (self.poison * 5)[:0x28])

    # x87 stack access: FP0..FP7 are physical registers, ST(i) = FP[(TOP+i)&7].
    def _st(self, i):
        top = (self.uc.reg_read(UC_X86_REG_FPSW) >> 11) & 7
        return (top + i) & 7

    def _fyl2x(self, uc, addr, size, _):
        p0, p1 = self._st(0), self._st(1)
        a, b = uc.reg_read(UC_X86_REG_FP0 + p0), uc.reg_read(UC_X86_REG_FP0 + p1)
        key = ('fyl2x', a, b)
        r = self.ext_cache.get(key)
        if r is None:
            x, y = ext_to_dec(a), ext_to_dec(b)
            r = self.ext_cache[key] = dec_to_ext(y * (x.ln() / LN2))
        uc.reg_write(UC_X86_REG_FP0 + p1, r)
        sw = uc.reg_read(UC_X86_REG_FPSW)  # pop: TOP+1, ST0's register becomes empty
        uc.reg_write(UC_X86_REG_FPSW, (sw & ~0x3800) | (p1 << 11))
        uc.reg_write(UC_X86_REG_FPTAG, uc.reg_read(UC_X86_REG_FPTAG) | (3 << (2 * p0)))
        uc.reg_write(UC_X86_REG_EIP, addr + 2)

    def _f2xm1(self, uc, addr, size, _):
        p0 = self._st(0)
        a = uc.reg_read(UC_X86_REG_FP0 + p0)
        key = ('f2xm1', a)
        r = self.ext_cache.get(key)
        if r is None:
            r = self.ext_cache[key] = dec_to_ext(Decimal(2) ** ext_to_dec(a) - 1)
        uc.reg_write(UC_X86_REG_FP0 + p0, r)
        uc.reg_write(UC_X86_REG_EIP, addr + 2)

    def _import(self, uc, addr, size, name):
        esp = uc.reg_read(UC_X86_REG_ESP)
        ret, a0, a1, a2, a3 = struct.unpack('<5I', uc.mem_read(esp, 20))
        res = 0
        if name == 'calloc':
            res = self.alloc(a0 * a1)
        elif name == 'malloc':
            res = self.alloc(a0, zero=False)
            uc.mem_write(res, (self.poison * (a0 // len(self.poison) + 1))[:a0])
        elif name == 'free':
            self.release(a0)
        elif name == 'strncpy':
            src = bytes(uc.mem_read(a1, a2))
            src = src.split(b'\0')[0][:a2]
            uc.mem_write(a0, src + bytes(a2 - len(src)))
            res = a0
        elif name == 'qsort':  # (base, n, width, cmp): only FUN_10005570's index sort
            idx = list(struct.unpack('<%dI' % a1, uc.mem_read(a0, 4 * a1)))
            key = lambda i: struct.unpack('<i', uc.mem_read(0x1000b338 + 72 * i, 4))[0]
            idx.sort(key=key)
            uc.mem_write(a0, struct.pack('<%dI' % a1, *idx))
        elif name == 'fread':  # (buf, size, count, FILE)
            f = self.files[a3]
            n = a1 * a2
            chunk = bytes(f[0][f[1]:f[1] + n])
            f[1] += len(chunk)
            uc.mem_write(a0, chunk)
            res = len(chunk) // a1 if a1 else 0
        elif name == 'fwrite':  # (buf, size, count, FILE)
            f = self.files[a3]
            data = bytes(uc.mem_read(a0, a1 * a2))
            end = f[1] + len(data)
            if end > len(f[0]):
                f[0].extend(bytes(end - len(f[0])))
            f[0][f[1]:end] = data
            f[1] = end
            res = a2
        elif name == 'ftell':
            res = self.files[a0][1]
        elif name == 'fseek':  # (FILE, offset, origin)
            f = self.files[a0]
            base = {0: 0, 1: f[1], 2: len(f[0])}[a2]
            f[1] = base + struct.unpack('<i', struct.pack('<I', a1))[0]
        elif name == 'time':
            res = self.now
        else:
            raise RuntimeError('plugin called %s' % name)
        uc.reg_write(UC_X86_REG_EAX, res)
        uc.reg_write(UC_X86_REG_ESP, esp + 4)
        uc.reg_write(UC_X86_REG_EIP, ret)

    # --- final render ----------------------------------------------------------

    def render(self, gdo, w, h, planes, channels, src, mask=None, max_space=0x7fffffff):
        """Filter an interleaved frame (planes bytes per pixel, the first
        `channels` colour) with a .gdo through the plugin's own filter
        selectors: FUN_10001890 (start: strip height from max_space, image
        mode → channel count and root type) and FUN_100010d0 (continue:
        one FUN_10001240 per strip). Returns the output frame bytes.

        The host's advanceState is a stub that clips each requested strip
        to the filter rectangle (an assumption about Photoshop) and points
        inData/outData/maskData at it."""
        assert len(src) == w * h * planes
        rect = (0, 0, h, w)  # top, left, bottom, right
        if mask is not None:
            ys = [i // w for i, m in enumerate(mask) if m]
            xs = [i % w for i, m in enumerate(mask) if m]
            if not xs:
                return bytes(src)
            rect = (min(ys), min(xs), max(ys) + 1, max(xs) + 1)
        top, left, bottom, right = rect

        # .gdo → record 0 via the plugin's own reader (no growth yet).
        self.files[READ_FILE] = [bytearray(gdo), 0]
        dims = self.alloc(20)
        self.call(0x100062a0, DAT['records'], 0, READ_FILE, dims, 0)
        self.put(DAT['record_index'], '<I', 0)

        inbuf, outbuf, maskbuf = HOST + 0x01000000, HOST + 0x02000000, HOST + 0x03000000
        self.uc.mem_write(inbuf, bytes(src))
        self.uc.mem_write(outbuf, bytes(src))
        if mask is not None:
            self.uc.mem_write(maskbuf, bytes(mask))
        row = w * planes
        fr = HOST
        stubs = HOST + 0x1000
        result = HOST + 0x1100                      # DAT_1000efb4 → selector result word
        self.uc.mem_write(stubs, b'\x33\xc0\xc3')    # abortProc: xor eax,eax; ret
        self.uc.mem_write(stubs + 4, b'\xc3')        # progressProc
        self.uc.mem_write(stubs + 8, b'\x90\x33\xc0\xc3')  # advanceState: nop (hook); xor eax,eax; ret
        self.uc.mem_write(result, b'\0\0')
        self.put(0x1000efb4, '<I', result)

        mode = {1: 1, 3: 3, 4: 4}[channels]         # Grayscale, RGBColor, CMYKColor
        rec = bytearray(0x200)
        struct.pack_into('<II', rec, 0x04, stubs, stubs + 4)
        struct.pack_into('<hh', rec, 0x10, h, w)    # imageSize
        struct.pack_into('<h', rec, 0x14, planes)
        struct.pack_into('<hhhh', rec, 0x16, top, left, bottom, right)  # filterRect
        struct.pack_into('<i', rec, 0x2c, max_space)
        if mask is not None:
            rec[0x5d] = 1
        struct.pack_into('<h', rec, 0x80, mode)
        struct.pack_into('<I', rec, 0xe0, stubs + 8)
        self.uc.mem_write(fr, bytes(rec))
        self.put(DAT['filter_record'], '<I', fr)
        self.strips = []

        def advance(uc, addr, size, _):
            t, l, b, r = struct.unpack('<hhhh', uc.mem_read(fr + 0x40, 8))
            b = min(b, bottom)  # host clips the strip to the filter rectangle
            for off in (0x34, 0x40, 0x60):
                uc.mem_write(fr + off, struct.pack('<hhhh', t, l, b, r))
            uc.mem_write(fr + 0x4c, struct.pack('<IiIi', inbuf + t * row + l * planes, row,
                                                outbuf + t * row + l * planes, row))
            if mask is not None:
                uc.mem_write(fr + 0x68, struct.pack('<Ii', maskbuf + t * w + l, w))
            self.strips.append((t, b))
        hook = self.uc.hook_add(UC_HOOK_CODE, advance, begin=stubs + 8, end=stubs + 8)

        if self.poison != b'\0':  # garbage below the caller's frame
            n = 0x100000 - 0x2000
            self.uc.mem_write(STACK - 0x100000, (self.poison * (n // len(self.poison) + 1))[:n])
        self.call(0x10001890)  # filterSelectorStart: strip height, mode
        self.call(0x100010d0)  # filterSelectorContinue: the strips
        self.uc.hook_del(hook)
        return bytes(self.uc.mem_read(outbuf, w * h * planes))

    # --- the Farm dialog -------------------------------------------------------

    def _dialog_host(self, w, h, image_type):
        """Globals the dialog relies on: the root type of new cells
        (DAT_1000a3ac), each cell's dims record (preview size; the
        generator limbs are filled by growth), and a 1×1 gray source for
        plasma children (they read pixels but draw no random numbers)."""
        self.put(DAT['root_type'], '<I', self.u32(DAT['type_image' if image_type else 'type_scalar'])[0])
        for i in range(9):
            self.put(0x1000a930 + 20 * i, '<ii', w, h)
        src = HOST + 0x01000000
        self.uc.mem_write(src, b'\x80\x80\x80')
        rec = bytearray(0x200)
        struct.pack_into('<h', rec, 0x14, 3)
        struct.pack_into('<hh', rec, 0x3c, 0, 2)
        struct.pack_into('<Ii', rec, 0x4c, src, 3)
        self.uc.mem_write(HOST, bytes(rec))
        self.put(DAT['filter_record'], '<I', HOST)
        self.put(0x1000a380, '<ii', 1, 1)

    def _save_session(self):
        self.files[WRITE_FILE] = [bytearray(), 0]
        self.call(0x10002ab0, WRITE_FILE)  # FUN_10007310 with the cell records
        return bytes(self.files[WRITE_FILE][0])

    def breed(self, gds, w, h, sliders, image_type=True):
        """The dialog's Breed command on a session: load it
        (FUN_10002a80), grow every cell from its seed (FUN_10002310),
        rate each active cell by its slider (FUN_100021a0), regrow each
        active cell (FUN_100021e0), and save (FUN_10002ab0). Returns the
        saved .gds bytes."""
        self._dialog_host(w, h, image_type)
        self.files[READ_FILE] = [bytearray(gds), 0]
        self.call(0x10002a80, READ_FILE)
        self.call(0x10002310)
        active = self.u32(0x1000a010)[0]
        for i in range(active):
            self.call(0x100021a0, i, sliders[i] if i < len(sliders) else 50)
        for i in range(active):
            self.call(0x100021e0, i)
        return self._save_session()

    def new_session(self, w, h, now, image_type=True):
        """The dialog's New Session (FUN_10002a20): seeds from time(),
        cells from the global grammar, written straight away. Returns the
        .gds bytes."""
        self._dialog_host(w, h, image_type)
        self.now = now
        self.files[WRITE_FILE] = [bytearray(), 0]
        self.call(0x10002a20, WRITE_FILE)
        return bytes(self.files[WRITE_FILE][0])
