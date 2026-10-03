package gdo

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	orig, err := os.ReadFile("testdata/render.gdo")
	if err != nil {
		t.Skipf("testdata missing: %v", err)
	}
	f, err := Parse(bytes.NewReader(orig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := Serialize(f)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	if len(out) != len(orig) {
		t.Errorf("size mismatch: serialized %d bytes, original %d", len(out), len(orig))
	}
	if !bytes.Equal(orig, out) {
		locateDiff(t, orig, out)
		t.Fatalf("round-trip is not byte-exact — see first divergence above")
	}
	t.Logf("round-trip OK: %d bytes, byte-exact", len(out))

	// Stage 2: re-parse the serialized bytes and deep-compare semantics.
	f2, err := Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if msg := filesEqual(f, f2); msg != "" {
		t.Errorf("semantic mismatch after round-trip: %s", msg)
	}

}

// filesEqual deep-compares two parsed files; returns "" if identical.
func filesEqual(a, b *File) string {
	if a.TagA != b.TagA || a.TagB != b.TagB || a.EOFCount != b.EOFCount {
		return fmt.Sprintf("tags differ: %+v vs %+v, EOFCount=%+v", a.TagA, b.TagB, a.EOFCount)
	}
	if a.Context.W != b.Context.W || a.Context.H != b.Context.H ||
		a.Context.Limbs != b.Context.Limbs {
		return "context blob differs"
	}
	if msg := listsEqual("first", a.FirstList, b.FirstList); msg != "" {
		return msg
	}
	if len(a.Objects) != len(b.Objects) {
		return fmt.Sprintf("object count differs: %d vs %d", len(a.Objects), len(b.Objects))
	}
	for i := range a.Objects {
		for slot := 0; slot < 12; slot++ {
			msg := listsEqual(fmt.Sprintf("object[%d].slot[%d]", i, slot),
				a.Objects[i].Slots[slot], b.Objects[i].Slots[slot])
			if msg != "" {
				return msg
			}
		}
	}
	return ""
}

func listsEqual(name string, a, b *ParamList) string {
	i := 0
	for ca, cb := a.Head, b.Head; ; ca, cb = ca.Next, cb.Next {
		aEnd := ca == nil
		bEnd := cb == nil
		if aEnd && bEnd {
			return ""
		}
		if aEnd != bEnd {
			return fmt.Sprintf("%s: length differs at entry %d", name, i)
		}
		if ca.ID != cb.ID || ca.RawWeight != cb.RawWeight {
			return fmt.Sprintf("%s: entry %d differs (id %d/%d w %04X/%04X)",
				name, i, ca.ID, cb.ID, ca.RawWeight, cb.RawWeight)
		}
		i++
	}
}

// locateDiff reports the first differing offset plus a hex window.
func locateDiff(t *testing.T, orig, out []byte) {
	t.Helper()
	n := len(orig)
	if len(out) < n {
		n = len(out)
	}
	for i := 0; i < n; i++ {
		if orig[i] != out[i] {
			lo := i - 16
			if lo < 0 {
				lo = 0
			}
			hi := i + 16
			if hi > len(orig) {
				hi = len(orig)
			}
			t.Logf("first divergence at offset 0x%04X (%d): orig=%02X out=%02X",
				i, i, orig[i], out[i])
			t.Logf("orig [%04X..%04X]: % X", lo, hi, orig[lo:hi])
			t.Logf("out  [%04X..%04X]: % X", lo, hi, out[lo:hi])
			return
		}
	}
	if len(orig) != len(out) {
		t.Logf("common prefix identical; lengths differ: orig=%d out=%d", len(orig), len(out))
	}
}
