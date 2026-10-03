package gdo

import (
	"bytes"
	"os"
	"testing"
)

// TestParseSingleCell tests parsing of a standalone .gdo file.
// Validates against known-good files from Photoshop exports.
func TestParseSingleCell(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		wantSize int // Expected file size (bytes)
		wantTagA uint16
		wantTagB uint16
		checkRNG bool // Whether RNG state is populated
	}{
		{
			name:     "render.gdo",
			filename: "testdata/render.gdo",
			wantSize: 7902, // Confirmed from hand-off docs
			wantTagA: 0x03E8,
			wantTagB: 9,
			checkRNG: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(tt.filename)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			if tt.wantSize > 0 && len(data) != tt.wantSize {
				t.Errorf("file size: got %d, want %d", len(data), tt.wantSize)
			}

			f, err := Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if f.TagA != tt.wantTagA {
				t.Errorf("TagA: got 0x%04X, want 0x%04X", f.TagA, tt.wantTagA)
			}
			if f.TagB != tt.wantTagB {
				t.Errorf("TagB: got %d, want %d", f.TagB, tt.wantTagB)
			}

			// Validate blob unpacking
			if f.Context.W == 0 || f.Context.H == 0 {
				t.Errorf("invalid dimensions: W=%d, H=%d", f.Context.W, f.Context.H)
			}

			if tt.checkRNG {
				rng := f.Context.RNG()
				if rng == 0 {
					t.Error("RNG state is zero (expected valid LCG state)")
				}
			}

			// Check object array existence
			if len(f.Objects) == 0 {
				t.Error("no objects in file")
			}

			// Verify slot structure
			for i, obj := range f.Objects {
				for j, slot := range obj.Slots[:] {
					if slot == nil {
						t.Errorf("object[%d].slot[%d] is nil", i, j)
					}
				}
			}

			t.Logf("%s", f.String())
		})
	}
}

func TestDumpFirstList(t *testing.T) {
	data, err := os.ReadFile("testdata/render.gdo")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for n := f.FirstList.Head; n != nil; n = n.Next {
		t.Logf("op=%d rawWeight=0x%04X weight=%.6f", n.ID, n.RawWeight, n.Weight)
	}
	for _, pair := range [][2]int{{0, 0}, {0, 5}, {3, 2}} {
		obj, slot := pair[0], pair[1]
		pool := f.Objects[obj].Slots[slot]
		t.Logf("--- object[%d].slot[%d] (parent=%d pos=%d depth=%d)", obj, slot, obj, slot/4, slot%4)
		for n := pool.Head; n != nil; n = n.Next {
			t.Logf("    op=%d rawWeight=0x%04X weight=%.6f", n.ID, n.RawWeight, n.Weight)
		}
	}
}

// minimalFile: tagA, tagB, 20-byte blob, root list of 2 entries, 0 objects,
// terminator.
func minimalFile(tagA uint16) []byte {
	b := []byte{byte(tagA), byte(tagA >> 8), 9, 0}
	b = append(b, make([]byte, 20)...)
	b = append(b, 2, 0, 7, 0, 0x00, 0x80, 8, 0, 0x00, 0x40) // (7, 0.5), (8, 0.25)
	return append(b, 0, 0, 0, 0)
}

// FUN_100063b0 builds exactly count nodes with a nil final next pointer;
// a trailing sentinel node would inflate the selector's fallback divisor.
func TestParamListNilTerminated(t *testing.T) {
	f, err := Parse(bytes.NewReader(minimalFile(0x03E8)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	n := 0
	for cur := f.FirstList.Head; cur != nil; cur = cur.Next {
		n++
	}
	if n != 2 || f.FirstList.Len() != 2 {
		t.Fatalf("root list has %d nodes (Len %d), want exactly 2", n, f.FirstList.Len())
	}
	if w := f.FirstList.Head.Weight; w != 0.5 {
		t.Fatalf("weight = %v, want 0.5", w)
	}
}

// FUN_100062a0 overwrites the first halfword with the second before
// using it, so any leading tag must be accepted.
func TestLeadingTagUnchecked(t *testing.T) {
	f, err := Parse(bytes.NewReader(minimalFile(0x1234)))
	if err != nil {
		t.Fatalf("Parse rejected tag 0x1234: %v", err)
	}
	out, err := Serialize(f)
	if err != nil || !bytes.Equal(out, minimalFile(0x1234)) {
		t.Fatalf("round-trip of non-0x03E8 tag failed: %v", err)
	}
}
