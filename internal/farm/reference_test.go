package farm

import (
	"image"
	"image/png"
	"os"
	"testing"
)

// The real plugin's render of render.gdo on a white 640×360 image
// (real_plugin/ffarm1.png).
func loadReference(t *testing.T) image.Image {
	t.Helper()
	fh, err := os.Open("../../real_plugin/ffarm1.png")
	if err != nil {
		t.Skipf("reference render missing: %v", err)
	}
	defer fh.Close()
	img, err := png.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func whiteFrame(w, h int) *Frame {
	f := &Frame{W: w, H: h, Planes: 3, Channels: 3, Pix: make([]byte, w*h*3)}
	for i := range f.Pix {
		f.Pix[i] = 255
	}
	return f
}

// The port reproduces the original plugin's render of render.gdo on a
// white 640×360 image pixel for pixel.
func TestReferenceBitExact(t *testing.T) {
	t.Parallel()
	ref := loadReference(t)
	got := NewEngine().Render(loadRender(t), whiteFrame(640, 360), nil)
	bad := 0
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			r, g, b, _ := ref.At(x, y).RGBA()
			p := got.at(x, y)
			if uint32(p[0]) != r>>8 || uint32(p[1]) != g>>8 || uint32(p[2]) != b>>8 {
				if bad < 5 {
					t.Errorf("(%d,%d): port %v, plugin (%d,%d,%d)", x, y, p, r>>8, g>>8, b>>8)
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d pixels differ from the plugin's render", bad, 640*360)
	}
}
