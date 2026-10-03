# ffarmgo internals

Reverse-engineering notes: how the port maps to the original `ffarm.8bf`,
file formats, the evolution algorithm, and how fidelity is verified.
For installation and usage see the [README](../README.md).

## Layout

```
ffarm.decompiled     Ghidra listing export of ffarm.8bf (the ground truth; not distributed)
tools/listing.py     query tool for the listing (disassembly, constants, xrefs, tables)
tools/emu.py         runs the plugin's own machine code under Unicorn (pip install unicorn)
tools/gen_vectors.py random filters + images rendered by the emulated plugin → test vectors
tools/gen_evolve.py  the plugin's New Session / Breed, emulated → evolution test vectors
tools/pe.py          maps the .8bf's sections to virtual addresses
real_plugin/         the original ffarm.8bf and its reference render (not distributed; see below)
cmd/ffarm            command-line filter: .gdo + image → PNG or TIFF
cmd/ffarmweb         browser front end for the Farm dialog (preview grid, rate, breed)
internal/batch       one filter over a folder of images (CLI folder mode, web batches)
internal/farm        node builder, prepare/eval, all operators, noise, plasma, renderer
internal/tiffw       baseline TIFF writer (gray, RGB, RGBA, CMYK)
internal/rng         48-bit LCG (drand48 family)
internal/gdo         .gdo reader/writer
internal/shaper      spline transfer curve used for shaped random draws
internal/x87         helpers for x87 FPU semantics (frndint, ftol, encode)
```

## What is verified against the binary

| Area | Functions | State |
|---|---|---|
| LCG `X' = 0x5DEECE66D·X + 0xB mod 2^48`, double and 31-bit readouts | 07de0, 07f70, 07fd0 | Verified |
| `.gdo` read/write | 062a0, 063b0, 06440, 060b0, 06150 | Verified; round-trips `render.gdo` byte for byte |
| Runtime type table (9 types) | 08030 and the init chain from 075b0 | Verified |
| Operator registry: order, names, result and child types | 05500, 054e0, seven registrars | Verified (`tools/listing.py ops`) |
| Roulette selector, including the exhausted-pool fallback | 055e0 | Verified |
| Tree growth, depth-4 leaves, abstract-type retry | 05850 | Verified (reference render, emulator vectors) |
| Random draws in operator constructors | operator ctors | Verified (`dither` draws none) |
| Node builder, size reconciliation, `pol2map` wrapping | 04920, 048c0, 05140, 051c0 | Ported |
| Prepare pass and per-pixel evaluation | 04d30, 04dd0, 04f30, 04f90 | Ported (grown trees have no shared nodes, so only constant subtrees are cached) |
| Scalar, image and coordinate operators | 07720 family, 046b0 family, 080d0, 08b80 | Ported |
| Noise (`dopnoise`, `dopbnoise`, `dopdnoise`) | 08300–088c0 | Ported |
| Plasma (`rfract`, `bfract`) | 06f70, 065b0–06b40 | Ported |
| `dither` | 08cd0, 08d30 | Ported |
| Source-image leaves, final render loop, strips | 04430, 04590, 01240, 053d0, 010d0, 01890 | Ported: grayscale, RGB and CMYK modes, transparency planes, selection masks, multi-strip rendering |
| Shaper formula and tables | 088d0, 08930, 08960 | Verified (all 32,768 inputs equal the x87 hardware model) |

**Precision.** Windows runs the x87 FPU with 53-bit precision control,
so the plugin's +, −, ×, ÷ round to double after each instruction and
float64 reproduces them (the port wraps products in `float64()` so Go
never fuses a multiply-add).

`FYL2X` and `F2XM1` produce 64-bit-mantissa results that the plugin then
rounds to 53 bits. `internal/x87/ext.go` models them as correctly
rounded to 64 bits, using double-double arithmetic with about 106 bits.
That matches real hardware except within about 2⁻⁶⁴ of a 53-bit rounding
boundary. Go's `math.Log`/`math.Expm1` differ in the last bit on 46% of
shaper inputs.

MSVCRT `pow` (x87-based) is modelled as correctly rounded (`x87.Pow`).
It first tries a cheap ~2^-72 evaluation and takes the full
double-double path only when that cannot settle the rounding, which
happens on about 0.04% of inputs.
Go's `math.Pow` is not correctly rounded on 32% of the plugin's inputs.

`_ftol` converts to a 64-bit integer and the plugin keeps the low 32
bits: NaN gives 0, and large values wrap.

`FSIN`/`FCOS` (polar points, which nothing produces) use Go's `math`.

**Render modes** (FUN_10001890, FUN_10003c00, FUN_10001240):
- The host's image mode sets the colour channel count (grayscale and
  duotone 1, RGB and Lab 3, CMYK 4). Only in three-channel modes does
  the output read an image value per channel; otherwise every channel
  gets the first component, so an image-rooted filter writes its red
  channel in Grayscale mode. The tree's root type still comes from the
  file.
- Sources with fewer than three planes are read as gray replicated to
  R, G and B by the source leaves.
- With a selection, the filter rectangle is the selection's bounding
  box: the tree is grown for that size, u and v span it, and the leaves
  see only that part of the image (mirror-tiled at its edges). Pixels
  with mask 0 are copied without being evaluated; the others are blended
  as v·m/255 + src·(1−m/255)/255 before encoding. Transparency planes of
  evaluated pixels become 255.
- The plugin processes the rectangle in horizontal strips sized from the
  host's `maxSpace` (FUN_10001890). One row costs (2·planes + 1)·width
  bytes, and rows are added until the strip reaches `maxSpace` (the
  crossing row included) or the full height.
- FUN_100010d0 then renders each strip as if it were the whole image:
  - the tree is regrown for the strip's size;
  - u and v span the strip;
  - the source leaves see, and mirror-tile, only the strip.
- `RenderStrips` (CLI `-maxspace`) reproduces this; `Render` is the
  single-strip case that any modern host gets.
- The plugin requests the last strip as top + rows even when that runs
  past the rectangle. The port assumes the host clips it to the filter
  rectangle; that is host behaviour, which the binary cannot show.

**Process-global state.** `dither` draws from a generator seeded once per
host process (`0x0018247E150C`) and advanced by every render, previews
included, so the port reproduces only a first render. The noise
permutation table is shuffled once from `srand48(123456790)` (`0x75BCD16`).

**Test coverage.** Growth of `render.gdo` (seed `0xE88C5F62A74A`) is
pinned at 35 draws and final state `0x9E3D5BEADB2F`. Synthetic tests
cover the paths that file doesn't reach:
- `farm/select_test.go`: the selector fallback and operator arity;
- `gdo/parser_test.go`: list termination.

The emulator vectors exercise growth on random filters.

## The `.gdo` format (as read by FUN_100062a0)

All fields are little-endian.

```
u16   tag            read and discarded (0x03E8 in known files)
u16   root type      runtime type ID of the tree root (9 = image)
20 B  context        copy of the cell's dims record: +0 u32 width, +4 u32
                     height (image size at the last save; 0 in a fresh
                     session), +8 u32 interpolation flag (always 0), +12
                     three u16 limbs of the 48-bit LCG seed (low limb
                     first), +18 padding
list  root pool      u16 count, then count × (u16 op, u16 weight)
u16   object count   N (22 in known files, one per operator slot)
N × 12 lists         pool for (parent op, child position 0..2, depth 0..3),
                     index = depth + 4·(3·parent + position)
u16   terminator     0
```

A weight `w` means `w / 65536`. The context seed is loaded into the
generator before growth.

## The `.gds` session format (FUN_10007310 writes, FUN_100073c0 reads)

A session is the Farm dialog's 3×3 grid. Each cell is a complete `.gdo`
record:

```
u16       version tag  1000 (ftol(atof("1")·1000 + 0.5))
i32       active       how many cells the dialog's preview thread
                       animates (DAT_1000a010; 3 at dialog start)
i32       selected     the cell the final filter renders (FUN_10007480);
                       −1 = none, and the filter then fails with −19
128 B     directory    session directory, NUL-terminated; bytes after
                       the NUL are stale (kept on rewrite)
u32 × 9   offsets      file offset of each cell
9 × cell  .gdo records back to back
```

A new session (FUN_10002a20) seeds each cell's generator with the
limbs (0x0165, time() & 0xffff, cell index), visible as seeds
`000N····0165` in a fresh session. `gdo.ParseSession` and
`gdo.WriteSession` round-trip both known sessions byte for byte. The
plugin's own readers, run under `tools/emu.py`, accept files written by
`WriteSession`.

## Grammar

Each operator returns one runtime type and takes up to three children:

| Slot | Name | Returns | Children |
|---|---|---|---|
| 0, 1 | `mu`, `mv` | scalar | — (the pixel's u = x/W, v = y/H) |
| 2 | `minpe` | scalar | scalar |
| 3–5 | `peplus`, `petimes`, `pepow` | scalar | scalar, scalar |
| 6, 7 | `switch`, `mix` | scalar | scalar ×3 |
| 8, 9 | `bias`, `gain` | scalar | scalar, scalar |
| 10 | `ri2cx` | map (type 7) | point (8), scalar |
| 11–13 | `dopnoise`, `dopbnoise`, `dopdnoise` | scalar / type 4 / point | scalar |
| 14, 15 | `rfract`, `bfract` | scalar / point | scalar |
| 16 | `dither` | scalar | scalar, scalar |
| 17 | `makrgb` | image | scalar ×3 |
| 18, 19 | `gswitch`, `gmix` | image | scalar, image, image |
| 20, 21 | `trfmap` | image / scalar | image or scalar, map |

Trees grow to depth 3. At depth 4, image children become the source
image, scalar children its luminance (both mirror-tiled past the edges),
point children a constant from two shaped draws, and map children
(type 7, abstract) are re-selected at the same depth.

What the operators do: `minpe` 1−a, `peplus` a+b−ab, `petimes` ab,
`pepow` aᵇ, `switch`/`gswitch` pick by a < ½, `mix`/`gmix` blend by a,
`bias`/`gain` are Perlin-style curves, `makrgb` packs a colour, `ri2cx`
builds a coordinate offset (u + 0.1·x·s, v + 0.1·y·s), `trfmap`
re-evaluates its first child at those coordinates, the noises are
Hermite gradient noise at (u, v)/s, `rfract`/`bfract` look up a
diamond-square plasma tile built at construction (64² for previews,
256² for the final render), and `dither` quantizes randomly to 4d+1
levels.

## Working with the listing

`ffarm.decompiled` is not part of the release. To use `tools/listing.py`,
export a Ghidra listing (disassembly and data bytes) of the original
`ffarm.8bf` to `ffarm.decompiled` in the project root.

```
python3 tools/listing.py dis 5850        # disassemble the function containing 0x10005850
python3 tools/listing.py f64 a2e8 16     # knot table as doubles
python3 tools/listing.py ops             # operator registry
python3 tools/listing.py callers 5850    # who calls the grower
python3 tools/listing.py --help
```

The first run parses the listing (a few seconds) and caches the result in
`.listing-cache.json`. Values the plugin computes at startup read as zero
in the image; recover them from the code that writes them (e.g. the
shaper's divisor and ln(V), written by FUN_100088d0).

## Evolution (the Farm dialog)

The plugin evolves filters through one global grammar kept in RAM. It is
never saved; every new cell takes a copy of it as its pools.

**Building the grammar** (FUN_10005ad0, at plugin load). Every
operator whose result type fits a slot gets the weight from this table
(DAT_1000a168), by its arity and the slot's depth; root pools use
depth 0. Two pairs are excluded: a noise's scale input may not be
`mu`, `mv` or `minpe`, and `minpe` may not feed `minpe`. Every pool is
then normalized.

| arity \ depth | 0 | 1 | 2 | 3 |
|---|---|---|---|---|
| 0 | 0.1 | 0.3 | 0.7 | 0.9 |
| 1 | 0.2 | 0.4 | 0.4 | 0.2 |
| 2 | 0.8 | 0.6 | 0.4 | 0.1 |
| 3 | 0.9 | 0.7 | 0.3 | 0.0 |

**Choice log.** Growth records every selection the roulette walk made
(not fallback picks) in the cell's choice log.

**Breed** (dialog handler 0x100037f1) runs one generation in two passes:
1. **Rate.** Each active cell is rated by its slider (0–100, default
   50): rating = (100 − slider)·0.01. With g = 4·(1 − 2·rating)³, every
   logged choice, most recent first, has its global pool weight mapped
   through `bias(w, g)` in float32 arithmetic, and the pool is
   renormalized (FUN_10005f20). Slider 0 favours the cell's choices, 100
   disfavours them, 50 leaves them alone.
2. **Regrow.** Each active cell is replaced by a new cell grown from the
   updated grammar (FUN_100056c0). The generator continues from the
   cell's own dims record, and the pre-growth seed goes into the new
   cell's blob.

**New Session** (FUN_10002a20) seeds cell i with the limbs (0x0165,
time() & 0xffff, i), copies the grammar into all nine cells, and saves
the file before growing anything.

**Saving weights.** A weight is written as floor(w·65536), via the
mantissa of float32(w + 1) (FUN_100061c0).

In the port, `farm.DefaultGrammar`, `Grammar.Learn`, `farm.NewSession`,
`farm.LoadFarm` and `Farm.Breed` implement this. The CLI's `-new` and
`-breed` expose them. `-grammar` keeps the grammar between runs; that
is a port extension, since the plugin loses it when Photoshop quits.

**Verification:**
- `farm.NewSession` reproduces the eight untouched cells of the real
  `render.gds` byte for byte; they were created with time = 0x80AC.
- `tools/gen_evolve.py` runs the plugin's own New Session and Breed
  (load, grow all, rate, regrow, save) in the emulator.
  `TestEvolveVectors` matches its saved sessions byte for byte. The
  cases cover two generations each with 1, 3 and 9 active cells,
  sliders 0–100, a scalar-root mode, and a new session.

## Preview front end

`cmd/ffarmweb` is a local web app that stands in for the Farm dialog. It
serves one session from memory, for one user, with no external
dependencies.

```
go run ./cmd/ffarmweb [-addr 127.0.0.1:9797] [-in photo.png] [-gds session.gds]
                      [-grammar g.json] [-state dir] [-size 131x71] [-preview 320x240]
```

- **Autosave.** After every change the server saves its state to
  `-state`, which defaults to `$XDG_STATE_HOME/ffarmweb` or else
  `~/.local/state/ffarmweb`. The state is the session (`session.gds`),
  the learned grammar (`grammar.json`), the source image as uploaded
  (`source`), and the sliders and generation count (`meta.json`). Files
  are replaced atomically. A restart restores all of it, so only the
  Undo history is lost. `-gds`, `-in` and `-grammar` override the
  restored parts, damaged state files are skipped with a log line, and
  `-state ""` turns autosave off. `weights.json` holds the cells' exact
  in-memory weights (see below), so a restart changes nothing at all,
  not even future breeding.
- **Undo** restores an exact copy of the farm (grammar, cells,
  generators, choice logs).

**In-memory and saved weights.** The dialog grows cells it has just
bred from float32 weights in RAM, while the `.gds` stores each weight
as floor(w·65536) and the plugin's final render reads the cell back
from the file (FUN_10007480). Now and then a roulette pick falls in
that 1/65536 sliver, and the saved cell grows a different tree from the
one the dialog showed; in the plugin, a dialog preview can disagree
with the final render for the same reason. The front end keeps
evolution exact (ratings use the in-memory trees, as the plugin does)
but renders previews, full renders and exports from the cell as saved.
What you see is therefore what the export, `ffarm -gds` and the plugin
produce.

- **Grid.** The nine cells are rendered by the port's engine. Click a
  picture to select the cell the filter renders; click it again to
  clear the selection.
- **Breed.** Each active cell has a slider. Right favours the cell's
  choices and left disfavours them; the page inverts the value into the
  dialog's 0 (favour) to 100 (disfavour) scale. Breed runs
  `Farm.Breed`, so only the active cells are rated and regrown. Press B
  to breed and U to undo; double-click a slider to reset it.
- **Undo, New session, Active.** Undo goes back up to 50 steps (session
  plus grammar). New session seeds from the current time. Active sets
  how many cells breed.
- **Files.** Open and save `.gds`, and export one cell as `.gdo`.
  **Load .gdo** under a cell replaces that cell with a filter file; you
  can also drop the file on the cell's picture. The cell is grown from
  its saved seed, as opening a session holding it would grow it, so it
  can be rated and bred like any other; Undo takes it back. Upload
  a source image (RGB, grayscale or CMYK; new cells get an image root
  only in RGB mode, as in the plugin).
- **Apply to a folder.** Choose a cell (the selected one by default),
  give the path of a folder **on the server** (a full path, or one
  starting with `~/`), and press **Apply cell N**. After a confirmation,
  every `.png` in that folder is **replaced** by its render: same name,
  same file permissions, no copies. Other files are left alone, and so
  are subfolders unless **Include subfolders** is ticked; then the PNGs
  at every depth below the folder are replaced the same way. Each file is written to a temporary file and renamed over the
  original, so an interrupted batch leaves every image either original
  or fully filtered, never half-written. The panel counts the PNGs as you
  type. The batch runs in the background with progress and a Cancel
  button, one at a time. Each image gets the same render as
  `ffarm -gds … -in image`, from the cell as saved.
- **Full render.** This is the plugin's final render of a cell over the
  full-size source, identical to `ffarm -gds … -cell N`.
- **Render panel.** Pick a cell, an aspect ratio (16:9, 4:3, 3:2, 1:1,
  5:4, 21:9, 2.39:1, and the portrait ratios 9:16, 4:5, 2:3, 3:4) and a
  format: SD 480, HD 720, Full HD 1080, 4K 2160 or 8K 4320 pixels on the
  short side. The long side is rounded to an even number, so 16:9 gives
  1920×1080 and 7680×4320, and 21:9 gives 2560×1080. Render crops the
  centre of the source to that aspect, resamples it (box filter down,
  bilinear up), runs the final render and downloads the PNG
  (`GET /api/export?cell=&aspect=&format=`). On a 12-core machine 4K
  takes about 1.3 s and 8K about 5 s; exports run one at a time.

**Multi-core rendering.** A strip's pixel loop is split across
goroutines (`Engine.Workers`; by default all CPUs from 65,536 pixels).
Each worker evaluates a copy of the prepared tree. The one sequential
piece, the dither generator, is jumped ahead (`rng.Skip`) to each
chunk's start, assuming every pixel draws the same number of values.
Every pixel checks that assumption, and the strip is re-rendered
sequentially if it fails. `TestParallelMatchesSequential` compares
both paths byte for byte, including the generator state left behind,
over 27 grown cells in RGB, gray + alpha and CMYK, with masks and strips.

**What is not the plugin.** Previews use the dialog's 64-point plasma
(`Engine.Preview`; DAT_1000a1d8 keeps its load-time 64 until a final
render sets 256). They render over a copy of the source box-scaled to
fit `-preview`, keeping its aspect. How the dialog's preview thread
samples the image is not modelled, and neither is its animation of the
active cells. Ratings, breeding and the saved files are the verified
`farm` code, and `cmd/ffarmweb`'s tests check that a breed through the
server equals `farm.Breed` and that Undo restores the session byte for
byte.

## Reference comparison

The `real_plugin/` folder is not part of the release. Put the original
`ffarm.8bf` and the reference render `ffarm1.png` there to run
`TestReferenceBitExact` (skipped otherwise) and the emulator tools.

`real_plugin/ffarm1.png` is the original plugin's render of `render.gdo`
on a white 640×360 image; `TestReferenceBitExact` reproduces all
230,400 pixels exactly.

How it got there: the first comparison matched the plasma (all 10
plasma-minimum dots at identical pixels) but not the noise. Fitting
free noise corners to the reference showed the interpolation and the
per-corner generator were right and pointed at the permutation table.
Running the plugin's own FUN_10008300 under `tools/emu.py` then showed
the table is seeded with `0x75BCD16` = 123456790; the port had used
123456789 (a decimal-conversion slip). With that fixed, the render is
bit-exact.

## Emulator-backed verification

`tools/emu.py` runs the original `real_plugin/ffarm.8bf` under the
Unicorn CPU emulator, driven through a fake Photoshop FilterRecord. It
runs the plugin's own init chain and `.gdo` reader, then its filter
selectors:
- **start** (FUN_10001890): strip height and image mode;
- **continue** (FUN_100010d0): one FUN_10001240 per strip, with a stub
  `advanceState` that clips each strip to the filter rectangle. The FPU starts with the Windows control word
0x27F.

The imports are stubbed as follows:
- `_ftol` and `floor` run as x87 machine code.
- `_CIpow` is correctly rounded, using exact decimal arithmetic.
- The memory and file functions are implemented in Python.

Unicorn's x87 core (old QEMU) computes `FYL2X` and `F2XM1` through host
doubles, which real hardware does not do. So the plugin's four such
instructions are intercepted and computed with the same correctly
rounded 64-bit model as the port, using exact decimal arithmetic.

The emulated plugin reproduces the real Photoshop render
`real_plugin/ffarm1.png` exactly (all 230,400 pixels), which validates
the emulator and its stubs for that workload. The plugin's own `Shape`,
run in the emulator, equals the hardware model on all 32,768 inputs.

`tools/gen_vectors.py` then writes random, type-correct `.gdo` filters
(every operator favoured in turn, selector fallback included), random
source frames in every mode (RGB, RGB + alpha, gray, gray + alpha,
CMYK), with and without selection masks, renders each through the
emulated plugin, and stores inputs and output in
`internal/farm/testdata/emu/`. `TestEmulatorVectors` renders the same
inputs with the port and requires byte-for-byte equality. There are 88
vectors:
- `v###`: 66 single-strip cases.
- `s###`: 22 multi-strip cases, made with `-strips`. Their small
  `maxSpace` values give 1 to h−1 rows per strip.

**Undefined behaviour in the original.** In two situations the plugin
reads uninitialized memory, so its output is not reproducible. The
generator excludes both:
- **Scalar root in an RGB image:** the render reads G and B from a stack
  buffer that holds only the scalar.
- **Plasma child with an understated flag:** a plasma's child declares
  itself coordinate-only (e.g. `dopbnoise`) but contains a pixel-reading
  leaf. The leaf then reads `ix`/`iy` from the plasma constructor's
  context, which the plasma never set. Those are stale values the plugin
  left on its own stack.

Each candidate is rendered twice with different garbage poisoned into
the stack, into malloc'd blocks and into that context. The candidate is
dropped if the outputs differ.

```
python3 -m venv venv && venv/bin/pip install unicorn
venv/bin/python tools/gen_vectors.py -n 66          # ~2 min reference check, then the vectors
FFARM_VECTORS=/some/dir go test ./internal/farm -run EmulatorVectors
```

Limits: `_CIpow`, `FYL2X` and `F2XM1` follow a model (correct rounding)
rather than the original MSVCRT code and real silicon. They agree except
in rare near-tie cases.

## Remaining work

1. More real Photoshop renders, to confirm the transcendental and
   `pow` models on silicon.

## Tests

```
go test ./...
```

