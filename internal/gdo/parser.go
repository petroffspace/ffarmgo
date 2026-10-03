// Package gdo implements bit-exact parsing of Filter Farm .gdo/.gds files.
// Format authority: Ghidra listings for ffarm.8bf (FUN_100062a0 reader chain).
package gdo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// File represents a single filter cell (.gdo) or grid cell (.gds slot).
// Mirrors the 0x44-byte RAM record written/read by FUN_100062a0/FUN_100060b0.
type File struct {
	TagA      uint16      // 0x03E8 (1000) in known files — read and DISCARDED by FUN_100062a0 (never checked)
	TagB      uint16      // Root type ID passed to FUN_10005850; 9 = DAT_1000f0cc (image type)
	Context   ContextBlob // 20-byte context (W, H, RNG state, padding)
	FirstList *ParamList  // First param list at blob→objects boundary
	Objects   []Object    // Object array, each with 12 param-list slots
	Global    *ParamList  // Second param list (global weights?)
	EOFCount  uint16      // Should be 0 (terminator)
}

// ContextBlob holds the 20-byte context region: a verbatim copy of the
// cell's dims record (DAT_1000a930 entry) made by FUN_10006500 when a
// session is created (FUN_10002a20 seeds each cell with limbs 0x0165,
// time() & 0xffff, cell index).
type ContextBlob struct {
	Raw [20]byte // Verbatim bytes (preserve for round-trip)
	W   uint32   // Image width at save time (little-endian u32 at +0)
	H   uint32   // Image height at save time (little-endian u32 at +4)
	// Bytes +8..+11: the dims record's interpolation flag (DAT_1000a938,
	// only ever 0); kept verbatim in Raw.
	Limbs [3]uint16 // 48-bit LCG state limbs at +12 (srand48 layout)
	// Bytes +18..+19: padding after the 6-byte generator state.
}

// RNG returns the packed 48-bit LCG state (for growth replay).
// Matches FUN_10007da0 limb packing: state = (limbs[2]<<32)|(limbs[1]<<16)|limbs[0]
func (c *ContextBlob) RNG() uint64 {
	return uint64(c.Limbs[2])<<32 | uint64(c.Limbs[1])<<16 | uint64(c.Limbs[0])
}

// Object represents one of the N objects in the cell record.
// Each owns exactly 12 param-list slots arranged [3×4] (position×depth).
type Object struct {
	Slots [12]*ParamList // Slot [a*4+b] where a∈{0,1,2}, b∈{0,1,2,3}
}

// ParamList is a singly-linked list of entries.
// Node layout (12 bytes): id@+0 (u16), weight@+4 (float32), next@+8 (ptr).
// On disk: u16 count, then count × (u16 id, u16 rawWeight).
type ParamList struct {
	Head *Node
}

// Node is one entry in a param-list linked chain.
type Node struct {
	ID        uint16
	RawWeight uint16  // Pre-conversion u16 (weight = raw / 65536.0)
	Weight    float32 // Computed weight (FUN_10006440 result)
	Next      *Node
}

// ErrUnexpectedEOF indicates the file ended prematurely.
var ErrUnexpectedEOF = errors.New("unexpected end of file")

// Parse reads a single .gdo cell record from r.
// Mirrors FUN_100062a0 read sequence exactly.
func Parse(r io.Reader) (*File, error) {
	f := &File{}

	// Read tagA (2 bytes)
	if err := binary.Read(r, binary.LittleEndian, &f.TagA); err != nil {
		return nil, fmt.Errorf("read tagA: %w", err)
	}
	// No check: FUN_100062a0 reads this halfword into the same stack slot
	// as TagB, overwriting it, so the plugin accepts any value here.

	// Read tagB (2 bytes)
	if err := binary.Read(r, binary.LittleEndian, &f.TagB); err != nil {
		return nil, fmt.Errorf("read tagB: %w", err)
	}

	// Read context blob (20 bytes) verbatim
	if _, err := io.ReadFull(r, f.Context.Raw[:]); err != nil {
		return nil, fmt.Errorf("read context blob: %w", err)
	}
	// Unpack W, H, limbs (little-endian throughout)
	f.Context.W = binary.LittleEndian.Uint32(f.Context.Raw[0:4])
	f.Context.H = binary.LittleEndian.Uint32(f.Context.Raw[4:8])
	f.Context.Limbs[0] = binary.LittleEndian.Uint16(f.Context.Raw[12:14])
	f.Context.Limbs[1] = binary.LittleEndian.Uint16(f.Context.Raw[14:16])
	f.Context.Limbs[2] = binary.LittleEndian.Uint16(f.Context.Raw[16:18])

	// Read first param list
	first, err := readParamList(r)
	if err != nil {
		return nil, fmt.Errorf("read first param list: %w", err)
	}
	f.FirstList = first

	// Read object count
	var objectCount uint16
	if err := binary.Read(r, binary.LittleEndian, &objectCount); err != nil {
		return nil, fmt.Errorf("read object count: %w", err)
	}

	// Read object array (N objects × 12 slots each)
	f.Objects = make([]Object, objectCount)
	for i := range f.Objects {
		obj := &f.Objects[i]
		for slot := 0; slot < 12; slot++ {
			list, err := readParamList(r)
			if err != nil {
				return nil, fmt.Errorf("read object[%d].slot[%d]: %w", i, slot, err)
			}
			obj.Slots[slot] = list
		}
	}

	// Read EOF terminator (should be 0)
	if err := binary.Read(r, binary.LittleEndian, &f.EOFCount); err != nil {
		return nil, fmt.Errorf("read terminator: %w", err)
	}
	// Note: Some files may have trailing data (globals/extra lists) —
	// not parsed here; caller can continue if needed.

	return f, nil
}

// readParamList mirrors FUN_100063b0: count, then count × (id, rawWeight),
// built as a NIL-TERMINATED chain of exactly count nodes (063b0 stores 0
// into the last node's next pointer at 0641f; there is no sentinel node —
// the selector's fallback divides by the number of nodes walked).
func readParamList(r io.Reader) (*ParamList, error) {
	var count uint16
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}

	pl := &ParamList{}
	link := &pl.Head
	for i := uint16(0); i < count; i++ {
		var id uint16
		var rawWeight uint16
		if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
			return nil, fmt.Errorf("entry[%d] id: %w", i, err)
		}
		if err := binary.Read(r, binary.LittleEndian, &rawWeight); err != nil {
			return nil, fmt.Errorf("entry[%d] rawWeight: %w", i, err)
		}

		// FUN_10006440 splices the halfword into a float's mantissa
		// (0x3F800000 | raw<<7 = 1 + raw/65536) and subtracts 1.0 — exact.
		n := &Node{ID: id, RawWeight: rawWeight, Weight: float32(rawWeight) / 65536.0}
		*link = n
		link = &n.Next
	}
	return pl, nil
}

// TotalNodes counts all entries across all param lists.
func (f *File) TotalNodes() int {
	n := 0
	for _, pl := range []*ParamList{f.FirstList, f.Global} {
		if pl != nil && pl.Head != nil {
			n += pl.Len()
		}
	}
	for _, obj := range f.Objects {
		for _, pl := range obj.Slots[:] {
			if pl != nil && pl.Head != nil {
				n += pl.Len()
			}
		}
	}
	return n
}

// Len returns the number of entries in this list.
func (pl *ParamList) Len() int {
	n := 0
	for cur := pl.Head; cur != nil; cur = cur.Next {
		n++
	}
	return n
}

// String formats the file structure (debug).
func (f *File) String() string {
	return fmt.Sprintf(
		"gdo.File{TagA=0x%04X, TagB=%d, W=%d, H=%d, RNG=0x%012X, Objects=%d, Nodes=%d}",
		f.TagA, f.TagB, f.Context.W, f.Context.H, f.Context.RNG(),
		len(f.Objects), f.TotalNodes(),
	)
}
