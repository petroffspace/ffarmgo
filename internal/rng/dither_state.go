package rng

// DitherState is the initial 48-bit state of the dither operator's
// generator: the static limbs at DAT_1000a370 (0x150C, 0x247E, 0x0018),
// read only by FUN_10008d30 and never reseeded. The generator therefore
// lives for the whole host process and advances on every dither
// evaluation, previews included.
const DitherState uint64 = 0x0018247E150C

// NoiseSeed is the srand48 seed FUN_10008300 uses to shuffle the noise
// permutation table once at plugin load.
const NoiseSeed uint32 = 0x75BCD16 // 123456790 (PUSH 0x75bcd16)
