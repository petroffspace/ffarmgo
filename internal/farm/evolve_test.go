package farm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"testing"

	"ffarmgo/internal/gdo"
)

func loadSessionFile(t *testing.T, path string) (*gdo.Session, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gdo.ParseSession(data)
	if err != nil {
		t.Fatal(err)
	}
	return s, data
}

// Cells 1–8 of the real render.gds were created by the plugin's New
// Session (time() & 0xffff = 0x80AC) and never bred: NewSession with the
// default grammar must reproduce them byte for byte. (Cell 0 was bred.)
func TestNewSessionMatchesRealCells(t *testing.T) {
	real, _ := loadSessionFile(t, "../gdo/testdata/render.gds")
	f := NewSession(DefaultGrammar(), TImage, 3, 0x80AC, Dims{})
	if f.Session.Active != 3 || f.Session.Selected != -1 || f.Session.DirString() != "" {
		t.Fatalf("header: active %d selected %d dir %q", f.Session.Active, f.Session.Selected, f.Session.DirString())
	}
	for i := 1; i < gdo.SessionCells; i++ {
		want, _ := gdo.Serialize(real.Cells[i])
		got, err := gdo.Serialize(f.Session.Cells[i])
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("cell %d differs from the real session (%v)", i, err)
		}
	}
}

func TestEncodeWeight(t *testing.T) {
	for raw := 0; raw < 0x10000; raw += 7 {
		if got := EncodeWeight(float32(raw) / 65536); got != uint16(raw) {
			t.Fatalf("EncodeWeight(%d/65536) = %d", raw, got)
		}
	}
	if EncodeWeight(1) != 0xFFFF || EncodeWeight(1.5) != 0xFFFF || EncodeWeight(0) != 0 {
		t.Fatal("EncodeWeight bounds")
	}
}

// A neutral rating (slider 50) leaves every weight unchanged up to the
// renormalization FUN_10005f20 still applies (the float sums are not
// exactly 1); a high rating raises the weights of the cell's choices.
func TestLearnDirection(t *testing.T) {
	real, _ := loadSessionFile(t, "../gdo/testdata/render.gds")
	f := LoadFarm(real, DefaultGrammar(), Dims{64, 48}, TImage)
	base := DefaultGrammar()
	f.Grammar.Learn(TImage, f.logs[0], Rating(50))
	for p := range base.Pools {
		for q := range base.Pools[p] {
			for d := range base.Pools[p][q] {
				a, b := base.Pools[p][q][d], f.Grammar.Pools[p][q][d]
				if len(a) != len(b) {
					t.Fatalf("slider 50 changed pool (%d,%d,%d)'s entries", p, q, d)
				}
				for i := range a {
					if math.Abs(float64(a[i].W-b[i].W)) > 1e-6 {
						t.Fatalf("slider 50 moved a weight: %v → %v", a[i].W, b[i].W)
					}
				}
			}
		}
	}
	if len(f.logs[0]) == 0 {
		t.Fatal("cell 0 grew without logging choices")
	}
	c := f.logs[0][len(f.logs[0])-1] // last choice: in a child pool
	before := f.Grammar.Pools[c.Parent][c.Pos][c.Depth].find(c.Op).W
	f.Grammar.Learn(TImage, f.logs[0], Rating(0)) // slider 0 → rating 1.0 → favour
	after := f.Grammar.Pools[c.Parent][c.Pos][c.Depth].find(c.Op).W
	if !(after > before) {
		t.Fatalf("favouring a choice did not raise its weight: %v → %v", before, after)
	}
}

// evolveVector is one case from tools/gen_evolve.py: the original
// plugin's Breed command (and optionally New Session), run under the
// emulator in one process so the global grammar carries over.
type evolveVector struct {
	Name       string
	W, H       int32
	Image      bool
	Input      string
	NewSession *uint32 `json:"new_session"`
	NewOut     string  `json:"new_out"`
	Gens       []struct {
		Sliders []int
		Out     string
	}
}

func TestEvolveVectors(t *testing.T) {
	fh, err := os.Open("testdata/evolve.json.gz")
	if err != nil {
		t.Skip(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	var cases []evolveVector
	if err := json.NewDecoder(zr).Decode(&cases); err != nil {
		t.Fatal(err)
	}
	write := func(s *gdo.Session) []byte {
		var buf bytes.Buffer
		if err := gdo.WriteSession(&buf, s); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			typ := TScalar
			if c.Image {
				typ = TImage
			}
			d := Dims{c.W, c.H}
			g := DefaultGrammar() // one plugin process: the grammar persists
			var data []byte
			if c.NewSession != nil {
				f := NewSession(g, typ, 3, *c.NewSession, d)
				data = write(f.Session)
				if !bytes.Equal(data, b64(t, c.NewOut)) {
					t.Fatal("New Session differs from the plugin's")
				}
			} else {
				data = b64(t, c.Input)
			}
			for i, gen := range c.Gens {
				s, err := gdo.ParseSession(data)
				if err != nil {
					t.Fatal(err)
				}
				f := LoadFarm(s, g, d, typ)
				f.Breed(gen.Sliders)
				data = write(f.Session)
				want := b64(t, gen.Out)
				if !bytes.Equal(data, want) {
					bad, first := 0, -1
					for j := range want {
						if j >= len(data) || data[j] != want[j] {
							if first < 0 {
								first = j
							}
							bad++
						}
					}
					t.Fatalf("generation %d (sliders %v): %d bytes differ (len %d vs %d), first at %d",
						i+1, gen.Sliders, bad, len(data), len(want), first)
				}
			}
		})
	}
}
