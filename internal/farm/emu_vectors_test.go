package farm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"ffarmgo/internal/gdo"
)

// emuVector is one case produced by tools/gen_vectors.py: inputs plus
// the output of the original plugin's final render (FUN_10001240) run
// under the Unicorn emulator (tools/emu.py).
type emuVector struct {
	Name     string
	W, H     int
	Planes   int
	Channels int
	GDO      string
	Src      string
	Mask     *string
	Out      string
	MaxSpace *int64 `json:"max_space"` // multi-strip vectors only
}

func b64(t *testing.T, s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// operators lists the node names of the tree the port grows, for
// diagnosing a mismatching vector.
func operators(n *Node, seen map[string]bool) {
	seen[n.Name] = true
	for _, k := range n.Kids {
		if k != nil {
			operators(k, seen)
		}
	}
}

func TestEmulatorVectors(t *testing.T) {
	dir := os.Getenv("FFARM_VECTORS")
	if dir == "" {
		dir = "testdata/emu"
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Skip("no emulator vectors (run tools/gen_vectors.py)")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var v emuVector
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			f, err := gdo.Parse(bytes.NewReader(b64(t, v.GDO)))
			if err != nil {
				t.Fatal(err)
			}
			src := &Frame{W: v.W, H: v.H, Planes: v.Planes, Channels: v.Channels, Pix: b64(t, v.Src)}
			var mask []byte
			if v.Mask != nil {
				mask = b64(t, *v.Mask)
			}
			want := b64(t, v.Out)
			var maxSpace int64
			if v.MaxSpace != nil {
				maxSpace = *v.MaxSpace
			}
			got := NewEngine().RenderStrips(f, src, mask, maxSpace).Pix
			if bytes.Equal(got, want) {
				return
			}
			bad, maxd, first := 0, 0, -1
			for i := range want {
				if got[i] != want[i] {
					if first < 0 {
						first = i
					}
					bad++
					d := int(got[i]) - int(want[i])
					if d < 0 {
						d = -d
					}
					maxd = max(maxd, d)
				}
			}
			e := NewEngine()
			e.src = rectSource{f: src, w: int32(v.W), h: int32(v.H)}
			e.PlasmaN = 256
			names := map[string]bool{}
			operators(e.Grow(f, Dims{int32(v.W), int32(v.H)}).Root, names)
			var ops []string
			for n := range names {
				ops = append(ops, n)
			}
			sort.Strings(ops)
			px := first / v.Planes
			t.Errorf("%d of %d bytes differ (max %d); first at pixel (%d,%d) plane %d: port %d, plugin %d; tree: %s",
				bad, len(want), maxd, px%v.W, px/v.W, first%v.Planes, got[first], want[first], strings.Join(ops, " "))
		})
	}
}
