package farm

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
)

// The parallel pixel loop must reproduce the sequential one byte for
// byte and leave the dither generator in the same state.
func TestParallelMatchesSequential(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	frame := func(w, h, planes, channels int) *Frame {
		f := &Frame{W: w, H: h, Planes: planes, Channels: channels, Pix: make([]byte, w*h*planes)}
		r.Read(f.Pix)
		return f
	}
	g := DefaultGrammar()
	var dithered atomic.Int32
	t.Cleanup(func() {
		if dithered.Load() == 0 {
			t.Error("no case exercised the dither generator")
		}
	})
	for s := 0; s < 3; s++ {
		typ := TImage
		if s == 2 {
			typ = TScalar
		}
		farm := NewSession(g, typ, 9, uint32(0x1000+s*977), Dims{W: 131, H: 71})
		for i, cell := range farm.Session.Cells {
			var src *Frame
			switch (s + i) % 4 {
			case 0, 1:
				src = frame(83, 57, 3, 3)
			case 2:
				src = frame(64, 40, 2, 1) // gray + alpha
			case 3:
				src = frame(50, 50, 4, 4) // CMYK
			}
			var mask []byte
			if i%3 == 1 {
				mask = make([]byte, src.W*src.H)
				for j := range mask {
					if r.Intn(4) != 0 {
						mask[j] = byte(r.Intn(256))
					}
				}
			}
			var maxSpace int64
			if i%4 == 3 {
				maxSpace = int64(src.W * (2*src.Planes + 1) * 20) // ~20-row strips
			}
			t.Run(fmt.Sprintf("s%d_cell%d", s, i), func(t *testing.T) {
				t.Parallel()
				seq, par := NewEngine(), NewEngine()
				seq.Workers, par.Workers = 1, 5
				for pass := 0; pass < 2; pass++ { // the second render inherits the generator
					a := seq.RenderStrips(cell, src, mask, maxSpace)
					b := par.RenderStrips(cell, src, mask, maxSpace)
					if !bytes.Equal(a.Pix, b.Pix) {
						t.Fatalf("pass %d: parallel render differs", pass)
					}
					if seq.dither.State() != par.dither.State() {
						t.Fatalf("pass %d: dither state %012X, want %012X",
							pass, par.dither.State(), seq.dither.State())
					}
				}
				if seq.dither.Draws() > 0 {
					dithered.Add(1)
				}
			})
		}
	}
}
