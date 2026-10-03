// Package tiffw writes baseline uncompressed TIFF files for gray, RGB,
// RGBA and CMYK images. The standard library has no CMYK encoder; TIFF
// lets CMYK filter results open in Photoshop as CMYK.
package tiffw

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
)

// Encode writes img as a single-strip, uncompressed, little-endian TIFF.
// *image.Gray, *image.CMYK and *image.NRGBA keep their channel layout;
// anything else is written as RGB.
func Encode(w io.Writer, img image.Image) error {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	var spp int
	var photometric uint16
	extra := false
	var pix []byte
	switch m := img.(type) {
	case *image.Gray:
		spp, photometric = 1, 1 // BlackIsZero
		for y := b.Min.Y; y < b.Max.Y; y++ {
			pix = append(pix, m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)]...)
		}
	case *image.CMYK:
		spp, photometric = 4, 5 // Separated (ink amounts)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			pix = append(pix, m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)]...)
		}
	case *image.NRGBA:
		spp, photometric, extra = 4, 2, true
		for y := b.Min.Y; y < b.Max.Y; y++ {
			pix = append(pix, m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)]...)
		}
	default:
		spp, photometric = 3, 2 // RGB
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				pix = append(pix, c.R, c.G, c.B)
			}
		}
	}
	if len(pix) != width*height*spp {
		return fmt.Errorf("tiffw: pixel data size mismatch")
	}

	type entry struct {
		tag, typ uint16
		count    uint32
		value    uint32
	}
	const (
		tShort = 3
		tLong  = 4
	)
	// Layout: header (8) | pixels | BitsPerSample array | IFD.
	pixOff := uint32(8)
	bpsOff := pixOff + uint32(len(pix))
	if bpsOff%2 != 0 {
		bpsOff++
	}
	ifdOff := bpsOff + uint32(2*spp)
	bps := uint32(8)
	if spp > 2 {
		bps = bpsOff // offset to an array of spp shorts
	}
	entries := []entry{
		{256, tLong, 1, uint32(width)},
		{257, tLong, 1, uint32(height)},
		{258, tShort, uint32(spp), bps},
		{259, tShort, 1, 1}, // no compression
		{262, tShort, 1, uint32(photometric)},
		{273, tLong, 1, pixOff},
		{277, tShort, 1, uint32(spp)},
		{278, tLong, 1, uint32(height)},
		{279, tLong, 1, uint32(len(pix))},
		{284, tShort, 1, 1}, // chunky
	}
	if extra {
		entries = append(entries, entry{338, tShort, 1, 2}) // unassociated alpha
	}
	le := binary.LittleEndian
	buf := []byte{'I', 'I', 42, 0}
	buf = le.AppendUint32(buf, ifdOff)
	buf = append(buf, pix...)
	for uint32(len(buf)) < bpsOff {
		buf = append(buf, 0)
	}
	for i := 0; i < spp; i++ {
		buf = le.AppendUint16(buf, 8)
	}
	buf = le.AppendUint16(buf, uint16(len(entries)))
	for _, e := range entries {
		buf = le.AppendUint16(buf, e.tag)
		buf = le.AppendUint16(buf, e.typ)
		buf = le.AppendUint32(buf, e.count)
		if e.typ == tShort && e.count == 1 {
			buf = le.AppendUint16(buf, uint16(e.value))
			buf = le.AppendUint16(buf, 0)
		} else {
			buf = le.AppendUint32(buf, e.value)
		}
	}
	buf = le.AppendUint32(buf, 0) // no next IFD
	_, err := w.Write(buf)
	return err
}
