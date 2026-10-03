package gdo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// SessionCells is the number of cells in a .gds session: the Farm
// dialog's 3×3 grid (FUN_100073c0 loops over the 9 offsets at
// DAT_1000f0a8..f0cc).
const SessionCells = 9

// Session is a .gds file: a farm session of nine cells.
//
// Layout (FUN_10007310 writes, FUN_100073c0 / FUN_10007480 read):
//
//	u16        version tag: ftol(atof("1")·1000 + 0.5) = 1000
//	0xAC bytes header (the block at DAT_1000f020):
//	  +0x00 i32   active cells: how many grid cells the dialog's preview
//	              thread animates (DAT_1000a010; 3 when the dialog starts)
//	  +0x04 i32   selected cell, rendered by the final filter
//	              (FUN_10007480); −1 = none (new session)
//	  +0x08 [128] session directory, NUL-terminated (FUN_10002ad0 strips
//	              the file name); bytes after the NUL are stale and kept
//	  +0x88 u32×9 file offset of each cell
//	9 × cell    each one a complete .gdo record (FUN_100060b0)
//
// The writer emits the header, then the cells while noting their
// ftell, then rewrites the header at offset 2 with the real offsets.
type Session struct {
	Tag      uint16
	Active   int32
	Selected int32
	Dir      [128]byte
	Cells    [SessionCells]*File
	Offsets  [SessionCells]uint32 // as read; WriteSession recomputes them
}

const sessionHeader = 2 + 0xAC

// DirString returns the session directory up to its NUL terminator.
func (s *Session) DirString() string {
	if i := bytes.IndexByte(s.Dir[:], 0); i >= 0 {
		return string(s.Dir[:i])
	}
	return string(s.Dir[:])
}

// SelectedCell returns the cell the plugin's final render uses, or an
// error when the session has none selected (the plugin then fails the
// filter with error −19, FUN_100015e0).
func (s *Session) SelectedCell() (*File, error) {
	if s.Selected < 0 || int(s.Selected) >= SessionCells {
		return nil, fmt.Errorf("session has no selected cell (index %d)", s.Selected)
	}
	return s.Cells[s.Selected], nil
}

// ParseSession reads a .gds file. Cells are read at their stored
// offsets, as FUN_100073c0 does.
func ParseSession(data []byte) (*Session, error) {
	if len(data) < sessionHeader {
		return nil, errors.New("gds: file shorter than its header")
	}
	s := &Session{Tag: binary.LittleEndian.Uint16(data[0:])}
	h := data[2:sessionHeader]
	s.Active = int32(binary.LittleEndian.Uint32(h[0x00:]))
	s.Selected = int32(binary.LittleEndian.Uint32(h[0x04:]))
	copy(s.Dir[:], h[0x08:0x88])
	for i := range s.Cells {
		s.Offsets[i] = binary.LittleEndian.Uint32(h[0x88+4*i:])
		off := int(s.Offsets[i])
		if off < sessionHeader || off >= len(data) {
			return nil, fmt.Errorf("gds: cell %d offset %d outside the file", i, off)
		}
		f, err := Parse(bytes.NewReader(data[off:]))
		if err != nil {
			return nil, fmt.Errorf("gds: cell %d: %w", i, err)
		}
		s.Cells[i] = f
	}
	return s, nil
}

// WriteSession writes a .gds file: header, then the nine cells back to
// back with offsets pointing at them — the file FUN_10007310 leaves.
func WriteSession(w io.Writer, s *Session) error {
	var cells [SessionCells][]byte
	off := uint32(sessionHeader)
	var offsets [SessionCells]uint32
	for i, c := range s.Cells {
		if c == nil {
			return fmt.Errorf("gds: cell %d is missing", i)
		}
		b, err := Serialize(c)
		if err != nil {
			return fmt.Errorf("gds: cell %d: %w", i, err)
		}
		cells[i] = b
		offsets[i] = off
		off += uint32(len(b))
	}
	out := make([]byte, sessionHeader, off)
	binary.LittleEndian.PutUint16(out[0:], s.Tag)
	h := out[2:]
	binary.LittleEndian.PutUint32(h[0x00:], uint32(s.Active))
	binary.LittleEndian.PutUint32(h[0x04:], uint32(s.Selected))
	copy(h[0x08:0x88], s.Dir[:])
	for i, o := range offsets {
		binary.LittleEndian.PutUint32(h[0x88+4*i:], o)
	}
	for _, b := range cells {
		out = append(out, b...)
	}
	_, err := w.Write(out)
	return err
}
