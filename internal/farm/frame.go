package farm

import (
	"image"
	"image/color"
)

// FrameFromImage converts a decoded image to a host-style frame:
// grayscale images give one colour channel, CMYK images four, everything
// else three (RGB); a transparency plane is added only when some pixel
// is not opaque.
//
// CMYK planes are stored the way Photoshop hands them to plug-ins:
// inverted, 255 = no ink (image.CMYK and TIFF store ink amounts).
func FrameFromImage(img image.Image) *Frame {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if c, ok := img.(*image.CMYK); ok {
		f := &Frame{W: w, H: h, Planes: 4, Channels: 4, Pix: make([]byte, 0, w*h*4)}
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				v := c.CMYKAt(x, y)
				f.Pix = append(f.Pix, 255-v.C, 255-v.M, 255-v.Y, 255-v.K)
			}
		}
		return f
	}
	gray := false
	switch img.(type) {
	case *image.Gray, *image.Gray16:
		gray = true
	}
	alpha := false
	for y := b.Min.Y; y < b.Max.Y && !alpha; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				alpha = true
				break
			}
		}
	}
	f := &Frame{W: w, H: h, Channels: 3}
	if gray {
		f.Channels = 1
	}
	f.Planes = f.Channels
	if alpha {
		f.Planes++
	}
	f.Pix = make([]byte, 0, w*h*f.Planes)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if gray {
				f.Pix = append(f.Pix, color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			} else {
				f.Pix = append(f.Pix, c.R, c.G, c.B)
			}
			if alpha {
				f.Pix = append(f.Pix, c.A)
			}
		}
	}
	return f
}

// Image converts the frame back: *image.CMYK for four-channel frames,
// *image.Gray for opaque grayscale, *image.RGBA for opaque RGB, and
// *image.NRGBA when there is a transparency plane.
func (f *Frame) Image() image.Image {
	if f.Channels == 4 {
		img := image.NewCMYK(image.Rect(0, 0, f.W, f.H))
		for y := 0; y < f.H; y++ {
			for x := 0; x < f.W; x++ {
				p := f.at(x, y)
				img.SetCMYK(x, y, color.CMYK{C: 255 - p[0], M: 255 - p[1], Y: 255 - p[2], K: 255 - p[3]})
			}
		}
		return img
	}
	if f.Channels == 1 && f.Planes == 1 {
		g := image.NewGray(image.Rect(0, 0, f.W, f.H))
		copy(g.Pix, f.Pix)
		return g
	}
	if f.Planes == f.Channels { // opaque: plain RGB
		img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
		for y := 0; y < f.H; y++ {
			for x := 0; x < f.W; x++ {
				p := f.at(x, y)
				img.SetRGBA(x, y, color.RGBA{p[0], p[min(1, f.Channels-1)], p[min(2, f.Channels-1)], 255})
			}
		}
		return img
	}
	img := image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			p := f.at(x, y)
			c := color.NRGBA{A: 255}
			if f.Channels >= 3 {
				c.R, c.G, c.B = p[0], p[1], p[2]
			} else {
				c.R, c.G, c.B = p[0], p[0], p[0]
			}
			if f.Planes > f.Channels {
				c.A = p[f.Channels]
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// MaskFromImage reads a selection mask: the gray level of each pixel.
func MaskFromImage(img image.Image) []byte {
	b := img.Bounds()
	m := make([]byte, 0, b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			m = append(m, color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
		}
	}
	return m
}
