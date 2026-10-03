package gdo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// Serialize renders a parsed File back to its on-disk byte stream.
// It is the exact inverse of Parse: field order, widths, and the
// list termination (nil-terminated chains of exactly count nodes) all
// mirror the reader.
func Serialize(f *File) ([]byte, error) {
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write streams the serialized form of f to w.
func Write(w io.Writer, f *File) error {
	// Fail fast if the parsed file had extra data — a nonzero EOFCount
	// means the parse landed on the wrong structure and emitting bytes
	// would silently produce a corrupt file.
	if f.EOFCount != 0 {
		return fmt.Errorf("write: EOFCount=%d, expected 0 — source file had unexpected trailing structure", f.EOFCount)
	}
	// Header
	if err := binary.Write(w, binary.LittleEndian, f.TagA); err != nil {
		return fmt.Errorf("write tagA: %w", err)
	}
	if err := binary.Write(w, binary.LittleEndian, f.TagB); err != nil {
		return fmt.Errorf("write tagB: %w", err)
	}

	// Context blob: typed fields for the decoded regions, Raw preserved
	// verbatim for the flag and padding (+8..+11, +18..+19). Writing the
	// typed fields makes File authoritative (mutations to W/H/Limbs are
	// honored rather than silently masked by stale Raw bytes).
	blob := f.Context.bytes()
	if _, err := w.Write(blob[:]); err != nil {
		return fmt.Errorf("write context blob: %w", err)
	}

	// First param list
	if err := writeParamList(w, f.FirstList); err != nil {
		return fmt.Errorf("write first param list: %w", err)
	}

	// Object array
	if err := binary.Write(w, binary.LittleEndian, uint16(len(f.Objects))); err != nil {
		return fmt.Errorf("write object count: %w", err)
	}
	for i := range f.Objects {
		for slot := 0; slot < 12; slot++ {
			pl := f.Objects[i].Slots[slot]
			if pl == nil {
				return fmt.Errorf("object[%d].slot[%d] is nil — cannot serialize", i, slot)
			}
			if err := writeParamList(w, pl); err != nil {
				return fmt.Errorf("write object[%d].slot[%d]: %w", i, slot, err)
			}
		}
	}

	// Terminator
	if err := binary.Write(w, binary.LittleEndian, f.EOFCount); err != nil {
		return fmt.Errorf("write terminator: %w", err)
	}

	// NOTE: File.Global is intentionally NOT written. The on-disk .gdo
	// format contains no second list (verified by byte accounting:
	// 28 + 265·2 + Σnodes·4 == file size). The field exists only in the
	// RAM representation.
	return nil
}

// bytes reconstructs the 20-byte context blob from the typed fields,
// carrying over the two unresolved regions from Raw verbatim.
func (c *ContextBlob) bytes() [20]byte {
	var b [20]byte
	binary.LittleEndian.PutUint32(b[0:4], c.W)
	binary.LittleEndian.PutUint32(b[4:8], c.H)
	copy(b[8:12], c.Raw[8:12]) // interpolation flag
	binary.LittleEndian.PutUint16(b[12:14], c.Limbs[0])
	binary.LittleEndian.PutUint16(b[14:16], c.Limbs[1])
	binary.LittleEndian.PutUint16(b[16:18], c.Limbs[2])
	copy(b[18:20], c.Raw[18:20]) // padding
	return b
}

// writeParamList emits: u16 count, then count × (u16 id, u16 rawWeight).
func writeParamList(w io.Writer, pl *ParamList) error {
	if err := binary.Write(w, binary.LittleEndian, uint16(pl.Len())); err != nil {
		return err
	}
	for cur := pl.Head; cur != nil; cur = cur.Next {
		if err := binary.Write(w, binary.LittleEndian, cur.ID); err != nil {
			return err
		}
		// RawWeight (u16), never the derived Weight float — the float is
		// a lossy presentation, the raw halfword is the disk truth.
		if err := binary.Write(w, binary.LittleEndian, cur.RawWeight); err != nil {
			return err
		}
	}
	return nil
}

// lists returns the record's parameter lists in file order.
func (f *File) lists() []*ParamList {
	l := []*ParamList{f.FirstList}
	for i := range f.Objects {
		l = append(l, f.Objects[i].Slots[:]...)
	}
	return append(l, f.Global)
}

// Weights returns every entry's in-memory weight, in file order. A
// record built in memory (the Farm dialog's new cells) carries float32
// weights that the file keeps only as RawWeight, floor(w·65536); a
// parsed record has Weight = RawWeight/65536.
func (f *File) Weights() []float32 {
	var w []float32
	for _, l := range f.lists() {
		if l == nil {
			continue
		}
		for n := l.Head; n != nil; n = n.Next {
			w = append(w, n.Weight)
		}
	}
	return w
}

// SetWeights restores weights saved by Weights. It reports false, and
// changes nothing, when w does not fit the record's entries.
func (f *File) SetWeights(w []float32) bool {
	if len(w) != len(f.Weights()) {
		return false
	}
	i := 0
	for _, l := range f.lists() {
		if l == nil {
			continue
		}
		for n := l.Head; n != nil; n = n.Next {
			n.Weight = w[i]
			i++
		}
	}
	return true
}
