// Command ffarmweb is a browser front end for the Farm dialog: a 3×3
// grid of cell previews rendered by the port's engine, a rating slider
// per active cell, Breed / New Session / Undo, cell selection, .gds open
// and save, and a full-size render of any cell.
//
//	ffarmweb [-addr 127.0.0.1:9797] [-in photo.png] [-gds session.gds] [-grammar g.json]
//
// It serves one session for one user. After every change it saves its
// state (session, learned grammar, source image, sliders) to the -state
// directory and restores it on the next start, so a restart loses only
// the Undo history. -gds, -in and -grammar override what is restored.
package main

import (
	"bytes"
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"ffarmgo/internal/batch"
	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
)

//go:embed index.html
var static embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:9797", "listen address")
	in := flag.String("in", "", "source image (PNG, JPEG or GIF); default: a built-in gradient")
	gds := flag.String("gds", "", "session to open (default: a new session)")
	grammar := flag.String("grammar", "", "load and save the evolving grammar here (port extension)")
	size := flag.String("size", "131x71", "the dialog's cell size, stored in new cells")
	box := flag.String("preview", "320x240", "largest preview size; previews keep the source's aspect")
	state := flag.String("state", defaultStateDir(), `directory the server saves its state to after every change and restores from at start; "" turns this off`)
	flag.Parse()

	s, err := newServer(options{in: *in, gds: *gds, grammar: *grammar, size: *size, box: *box, state: *state})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ffarmweb:", err)
		os.Exit(1)
	}
	if *state != "" {
		log.Printf("state: %s", *state)
	}
	log.Printf("Filter Farm preview on http://%s/", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.routes()))
}

// snapshot is one undo step: an exact copy of the farm (grammar,
// cells, generators, choice logs) and the page state before a change.
type snapshot struct {
	farm    *farm.Farm
	sliders [gdo.SessionCells]int
	breeds  int
}

type server struct {
	mu          sync.Mutex
	farm        *farm.Farm
	grammarPath string
	stateDir    string      // "" = no autosave
	src         *farm.Frame // full-size source
	srcName     string
	srcData     []byte      // the source file as uploaded; nil for the built-in one
	small       *farm.Frame // source scaled to the preview size
	cellSize    farm.Dims
	box         farm.Dims
	sliders     [gdo.SessionCells]int // the dialog's sliders: 0 favour, 50 neutral, 100 disfavour
	srcRev      int                   // bumped when the source changes
	breeds      int
	history     []snapshot
	ids         map[*gdo.File]int // revision id of each cell object
	nextID      int
	cache       map[cacheKey][]byte // preview PNGs
	batch       batch.Runner
}

type cacheKey struct {
	cell   *gdo.File
	srcRev int
}

// options are the command-line settings newServer starts from.
type options struct {
	in, gds, grammar string
	size, box        string
	state            string
}

// meta is the part of the saved state that has no file format of its
// own.
type meta struct {
	Source  string                `json:"source"` // display name of state/source
	Sliders [gdo.SessionCells]int `json:"sliders"`
	Breeds  int                   `json:"breeds"`
}

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "ffarmweb")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "ffarmweb")
}

// newServer starts from the saved state where there is one, with the
// options' files taking precedence.
func newServer(o options) (*server, error) {
	s := &server{stateDir: o.state, cache: map[cacheKey][]byte{}, ids: map[*gdo.File]int{}}
	var err error
	if s.cellSize, err = parseDims(o.size); err != nil {
		return nil, err
	}
	if s.box, err = parseDims(o.box); err != nil {
		return nil, err
	}
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
			return nil, fmt.Errorf("state directory: %w", err)
		}
	}
	var m meta
	haveMeta := s.readState("meta.json", func(b []byte) error { return json.Unmarshal(b, &m) })

	switch {
	case o.in != "":
		data, err := os.ReadFile(o.in)
		if err != nil {
			return nil, err
		}
		if err := s.setSource(data, o.in); err != nil {
			return nil, err
		}
	case s.readState("source", func(b []byte) error { return s.setSource(b, cmp.Or(m.Source, "saved source")) }):
	default:
		s.setFrame(defaultSource(), "built-in gradient")
	}

	s.grammarPath = o.grammar
	if s.grammarPath == "" && s.stateDir != "" {
		s.grammarPath = filepath.Join(s.stateDir, "grammar.json")
	}
	g, err := loadGrammar(s.grammarPath)
	if err != nil {
		return nil, err
	}

	s.resetSliders()
	var ss *gdo.Session
	if o.gds != "" {
		data, err := os.ReadFile(o.gds)
		if err != nil {
			return nil, err
		}
		if ss, err = gdo.ParseSession(data); err != nil {
			return nil, fmt.Errorf("%s: %w", o.gds, err)
		}
	} else if s.readState("session.gds", func(b []byte) (err error) { ss, err = gdo.ParseSession(b); return }) {
		if haveMeta {
			s.sliders, s.breeds = m.Sliders, m.Breeds
		}
		// The .gds rounds the weights of cells bred in memory; the
		// exact ones make the regrown cells (and so their choice logs
		// and later breeding) what they were before the restart.
		s.readState("weights.json", func(b []byte) error {
			var w [gdo.SessionCells][]float32
			if err := json.Unmarshal(b, &w); err != nil {
				return err
			}
			for i, c := range ss.Cells {
				if len(w[i]) != len(c.Weights()) {
					return fmt.Errorf("cell %d does not match the session", i)
				}
			}
			for i, c := range ss.Cells {
				c.SetWeights(w[i])
			}
			return nil
		})
	}
	if ss != nil {
		s.farm = farm.LoadFarm(ss, g, s.cellSize, s.cellType())
	} else {
		s.farm = farm.NewSession(g, s.cellType(), 3, uint32(time.Now().Unix()), s.cellSize)
	}
	s.saveSource()
	s.persist()
	return s, nil
}

// readState reads one saved file and hands it to use; it reports false
// when autosave is off, the file is missing, or use rejects it (a
// damaged file is logged and ignored rather than blocking the start).
func (s *server) readState(name string, use func([]byte) error) bool {
	if s.stateDir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(s.stateDir, name))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("state: %v", err)
		}
		return false
	}
	if err := use(b); err != nil {
		log.Printf("state: ignoring %s: %v", name, err)
		return false
	}
	return true
}

// persist saves the session, grammar and meta; callers hold s.mu (or
// own s). Failures are logged: the session goes on in memory.
func (s *server) persist() {
	if err := saveGrammar(s.grammarPath, s.farm.Grammar); err != nil {
		log.Printf("state: grammar: %v", err)
	}
	if s.stateDir == "" {
		return
	}
	var buf bytes.Buffer
	if err := gdo.WriteSession(&buf, s.farm.Session); err != nil {
		log.Printf("state: session: %v", err)
	} else if err := writeAtomic(filepath.Join(s.stateDir, "session.gds"), buf.Bytes()); err != nil {
		log.Printf("state: %v", err)
	}
	var weights [gdo.SessionCells][]float32
	for i, c := range s.farm.Session.Cells {
		weights[i] = c.Weights()
	}
	if b, err := json.Marshal(weights); err != nil {
		log.Printf("state: weights: %v", err)
	} else if err := writeAtomic(filepath.Join(s.stateDir, "weights.json"), b); err != nil {
		log.Printf("state: %v", err)
	}
	m, _ := json.Marshal(meta{Source: s.srcName, Sliders: s.sliders, Breeds: s.breeds})
	if err := writeAtomic(filepath.Join(s.stateDir, "meta.json"), m); err != nil {
		log.Printf("state: %v", err)
	}
}

// saveSource saves the source file as uploaded, or removes the saved
// one when the source is the built-in gradient.
func (s *server) saveSource() {
	if s.stateDir == "" {
		return
	}
	path := filepath.Join(s.stateDir, "source")
	var err error
	if s.srcData == nil {
		if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = writeAtomic(path, s.srcData)
	}
	if err != nil {
		log.Printf("state: source: %v", err)
	}
}

// writeAtomic replaces path with data via a temporary file and rename,
// so a crash leaves either the old file or the new one.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (s *server) routes() http.Handler {
	m := http.NewServeMux()
	m.Handle("GET /", http.FileServerFS(static))
	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("GET /api/cell/{i}", s.handlePreview)
	m.HandleFunc("GET /api/render/{i}", s.handleRender)
	m.HandleFunc("GET /api/gdo/{i}", s.handleGDO)
	m.HandleFunc("POST /api/gdo/{i}", s.handleLoadGDO)
	m.HandleFunc("GET /api/export", s.handleExport)
	m.HandleFunc("GET /api/batch", s.handleBatchStatus)
	m.HandleFunc("GET /api/batch/list", s.handleBatchList)
	m.HandleFunc("POST /api/batch", s.handleBatchStart)
	m.HandleFunc("POST /api/batch/cancel", s.handleBatchCancel)
	m.HandleFunc("GET /api/session.gds", s.handleSave)
	m.HandleFunc("POST /api/new", s.post(s.doNew))
	m.HandleFunc("POST /api/breed", s.post(s.doBreed))
	m.HandleFunc("POST /api/undo", s.post(s.doUndo))
	m.HandleFunc("POST /api/select", s.post(s.doSelect))
	m.HandleFunc("POST /api/active", s.post(s.doActive))
	m.HandleFunc("POST /api/open", s.upload(s.doOpen))
	m.HandleFunc("POST /api/source", s.upload(s.doSource))
	return m
}

// ---- source image ----

func (s *server) setSource(data []byte, name string) error {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	s.setFrame(farm.FrameFromImage(img), name)
	s.srcData = data
	return nil
}

func (s *server) setFrame(f *farm.Frame, name string) {
	s.src, s.srcName, s.srcData = f, filepath.Base(name), nil
	w, h := fit(f.W, f.H, int(s.box.W), int(s.box.H))
	s.small = scale(f, w, h)
	s.srcRev++
}

// cellType is the root type of new cells: DAT_1000a3ac, the image type
// in RGB mode and the scalar type otherwise.
func (s *server) cellType() int {
	if s.src.Channels == 3 {
		return farm.TImage
	}
	return farm.TScalar
}

func modeName(f *farm.Frame) string {
	m := map[int]string{1: "Grayscale", 3: "RGB", 4: "CMYK"}[f.Channels]
	if f.Planes > f.Channels {
		m += " + alpha"
	}
	return m
}

// fit returns the largest size with w:h's aspect inside bw×bh, never
// larger than w×h.
func fit(w, h, bw, bh int) (int, int) {
	if w <= bw && h <= bh {
		return w, h
	}
	if w*bh > h*bw {
		return bw, max(1, h*bw/w)
	}
	return max(1, w*bh/h), bh
}

// scale resamples a frame (box average when shrinking).
func scale(f *farm.Frame, w, h int) *farm.Frame {
	out := &farm.Frame{W: w, H: h, Planes: f.Planes, Channels: f.Channels, Pix: make([]byte, w*h*f.Planes)}
	sum := make([]int, f.Planes)
	for y := 0; y < h; y++ {
		y0, y1 := y*f.H/h, max((y+1)*f.H/h, y*f.H/h+1)
		for x := 0; x < w; x++ {
			x0, x1 := x*f.W/w, max((x+1)*f.W/w, x*f.W/w+1)
			clear(sum)
			for sy := y0; sy < y1; sy++ {
				row := f.Pix[(sy*f.W+x0)*f.Planes : (sy*f.W+x1)*f.Planes]
				for i, v := range row {
					sum[i%f.Planes] += int(v)
				}
			}
			n := (y1 - y0) * (x1 - x0)
			o := out.Pix[(y*w+x)*f.Planes:]
			for p := range sum {
				o[p] = byte((sum[p] + n/2) / n)
			}
		}
	}
	return out
}

// defaultSource is a soft colour gradient, so leaves that read the
// image show something more than a flat field.
func defaultSource() *farm.Frame {
	const w, h = 640, 360
	f := &farm.Frame{W: w, H: h, Planes: 3, Channels: 3, Pix: make([]byte, 0, w*h*3)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f.Pix = append(f.Pix, byte(64+x*160/w), byte(64+y*160/h), byte(200-(x+y)*120/(w+h)))
		}
	}
	return f
}

// ---- state ----

type stateJSON struct {
	SrcRev   int                        `json:"srcRev"`
	Breeds   int                        `json:"breeds"`
	Active   int                        `json:"active"`
	Selected int                        `json:"selected"`
	Sliders  [gdo.SessionCells]int      `json:"sliders"`
	Cells    [gdo.SessionCells]cellJSON `json:"cells"`
	Source   string                     `json:"source"`
	SrcW     int                        `json:"srcW"`
	SrcH     int                        `json:"srcH"`
	Mode     string                     `json:"mode"`
	PreviewW int                        `json:"previewW"`
	PreviewH int                        `json:"previewH"`
	CanUndo  bool                       `json:"canUndo"`
	Aspects  []option                   `json:"aspects"`
	Formats  []option                   `json:"formats"`
	Sizes    map[string][2]int          `json:"sizes"` // "aspect|format" → width, height
	Dir      string                     `json:"dir"`
}

type cellJSON struct {
	Seed string `json:"seed"`
	Root string `json:"root"`
	Rev  int    `json:"rev"`
	Size string `json:"size,omitempty"` // the size saved in the cell, if any
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.stateLocked())
}

func (s *server) stateLocked() stateJSON {
	ss := s.farm.Session
	st := stateJSON{
		SrcRev: s.srcRev, Breeds: s.breeds, Active: int(ss.Active), Selected: int(ss.Selected),
		Sliders: s.sliders, Source: s.srcName, SrcW: s.src.W, SrcH: s.src.H, Mode: modeName(s.src),
		PreviewW: s.small.W, PreviewH: s.small.H, CanUndo: len(s.history) > 0, Dir: ss.DirString(),
		Sizes: map[string][2]int{},
	}
	for _, a := range aspects {
		st.Aspects = append(st.Aspects, option{a.id, a.label})
		for _, f := range formats {
			w, h := exportSize(a, f)
			st.Sizes[a.id+"|"+f.id] = [2]int{w, h}
		}
	}
	for _, f := range formats {
		st.Formats = append(st.Formats, option{f.id, f.label})
	}
	for i, c := range ss.Cells {
		root := "scalar"
		if c.TagB == farm.TImage {
			root = "image"
		}
		st.Cells[i] = cellJSON{Seed: fmt.Sprintf("%012X", c.Context.RNG()), Root: root, Rev: s.id(c)}
		if c.Context.W != 0 && c.Context.H != 0 {
			st.Cells[i].Size = fmt.Sprintf("%d×%d", c.Context.W, c.Context.H)
		}
	}
	return st
}

// id returns a cell object's revision id; callers hold s.mu.
func (s *server) id(c *gdo.File) int {
	if _, ok := s.ids[c]; !ok {
		s.nextID++
		s.ids[c] = s.nextID
	}
	return s.ids[c]
}

// changed drops cached previews and ids of cells no longer in the
// session; callers hold s.mu.
func (s *server) changed() {
	live := map[*gdo.File]bool{}
	for _, c := range s.farm.Session.Cells {
		live[c] = true
	}
	for k := range s.cache {
		if !live[k.cell] || k.srcRev != s.srcRev {
			delete(s.cache, k)
		}
	}
	for c := range s.ids {
		if !live[c] {
			delete(s.ids, c)
		}
	}
}

func (s *server) resetSliders() {
	for i := range s.sliders {
		s.sliders[i] = 50
	}
}

// push records an undo step; callers hold s.mu.
func (s *server) push() error {
	s.history = append(s.history, snapshot{farm: s.farm.Clone(), sliders: s.sliders, breeds: s.breeds})
	if len(s.history) > 50 {
		s.history = s.history[1:]
	}
	return nil
}

// ---- actions ----

type request struct {
	Active  int                   `json:"active"`
	Cell    int                   `json:"cell"`
	Sliders [gdo.SessionCells]int `json:"sliders"`
}

// post wraps an action: decode the JSON body, run it under the lock,
// answer with the new state.
func (s *server) post(fn func(request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := fn(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.persist()
		writeJSON(w, s.stateLocked())
	}
}

// upload wraps an action that takes one uploaded file ("file").
func (s *server) upload(fn func(data []byte, name string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 256<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := fn(data, hdr.Filename); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.persist()
		writeJSON(w, s.stateLocked())
	}
}

func (s *server) doNew(req request) error {
	active := req.Active
	if active < 1 || active > gdo.SessionCells {
		active = 3
	}
	if err := s.push(); err != nil {
		return err
	}
	s.farm = farm.NewSession(s.farm.Grammar, s.cellType(), int32(active), uint32(time.Now().Unix()), s.cellSize)
	s.resetSliders()
	s.breeds = 0
	s.changed()
	return nil
}

func (s *server) doBreed(req request) error {
	for _, v := range req.Sliders {
		if v < 0 || v > 100 {
			return errors.New("sliders run 0-100")
		}
	}
	if err := s.push(); err != nil {
		return err
	}
	s.sliders = req.Sliders
	s.farm.Type = s.cellType()
	s.farm.Breed(s.sliders[:])
	s.resetSliders()
	s.breeds++
	s.changed()
	return nil
}

func (s *server) doUndo(request) error {
	if len(s.history) == 0 {
		return errors.New("nothing to undo")
	}
	h := s.history[len(s.history)-1]
	s.history = s.history[:len(s.history)-1]
	s.farm, s.sliders, s.breeds = h.farm, h.sliders, h.breeds
	s.changed()
	return nil
}

func (s *server) doSelect(req request) error {
	if req.Cell < -1 || req.Cell >= gdo.SessionCells {
		return errors.New("cell out of range")
	}
	s.farm.Session.Selected = int32(req.Cell)
	return nil
}

func (s *server) doActive(req request) error {
	if req.Active < 1 || req.Active > gdo.SessionCells {
		return fmt.Errorf("active cells run 1-%d", gdo.SessionCells)
	}
	s.farm.Session.Active = int32(req.Active)
	return nil
}

func (s *server) doOpen(data []byte, name string) error {
	ss, err := gdo.ParseSession(data)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := s.push(); err != nil {
		return err
	}
	s.farm = farm.LoadFarm(ss, s.farm.Grammar, s.cellSize, s.cellType())
	s.resetSliders()
	s.breeds = 0
	s.changed()
	return nil
}

// handleLoadGDO replaces one cell with an uploaded .gdo filter.
func (s *server) handleLoadGDO(w http.ResponseWriter, r *http.Request) {
	i, ok := s.cellIndex(w, r)
	if !ok {
		return
	}
	s.upload(func(data []byte, name string) error { return s.doLoadCell(i, data, name) })(w, r)
}

func (s *server) doLoadCell(i int, data []byte, name string) error {
	c, err := gdo.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if c.TagB != farm.TImage && c.TagB != farm.TScalar {
		return fmt.Errorf("%s: root type %d is not a filter's (scalar %d or image %d)", name, c.TagB, farm.TScalar, farm.TImage)
	}
	if err := s.push(); err != nil {
		return err
	}
	s.farm.SetCell(i, c)
	s.sliders[i] = 50
	s.changed()
	return nil
}

func (s *server) doSource(data []byte, name string) error {
	if err := s.setSource(data, name); err != nil {
		return err
	}
	s.saveSource()
	s.changed()
	return nil
}

// ---- images and files ----

// saved returns a cell as its file holds it. Cells bred in memory carry
// float32 weights that the file rounds down to 1/65536; the plugin's
// final render reads the cell back from the .gds (FUN_10007480), so
// everything the server renders uses this form, and a preview shows
// what the export, `ffarm -gds` and the plugin produce.
func saved(c *gdo.File) *gdo.File {
	b, err := gdo.Serialize(c)
	if err == nil {
		if p, err := gdo.Parse(bytes.NewReader(b)); err == nil {
			return p
		}
	}
	return c
}

func (s *server) cellIndex(w http.ResponseWriter, r *http.Request) (int, bool) {
	i, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("i"), ".png"))
	if err != nil || i < 0 || i >= gdo.SessionCells {
		http.Error(w, "cell must be 0-8", http.StatusBadRequest)
		return 0, false
	}
	return i, true
}

// handlePreview renders one cell over the scaled source. Renders run
// outside the lock (each on its own engine), so the browser's nine
// requests render in parallel.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	i, ok := s.cellIndex(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	cell, src := s.farm.Session.Cells[i], s.small
	key := cacheKey{cell, s.srcRev}
	cached := s.cache[key]
	s.mu.Unlock()
	if cached == nil {
		img := farm.NewEngine().Preview(saved(cell), src).Image()
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cached = buf.Bytes()
		s.mu.Lock()
		if s.farm.Session.Cells[i] == cell && s.srcRev == key.srcRev {
			s.cache[key] = cached
		}
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(cached)
}

// handleRender is the plugin's final render of a cell over the full
// source, as `ffarm -gds … -cell N` does.
func (s *server) handleRender(w http.ResponseWriter, r *http.Request) {
	i, ok := s.cellIndex(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	cell, src := s.farm.Session.Cells[i], s.src
	s.mu.Unlock()
	img := farm.NewEngine().Render(saved(cell), src, nil).Image()
	w.Header().Set("Content-Type", "image/png")
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ffarm-cell%d.png"`, i))
	}
	png.Encode(w, img)
}

func (s *server) handleGDO(w http.ResponseWriter, r *http.Request) {
	i, ok := s.cellIndex(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	b, err := gdo.Serialize(s.farm.Session.Cells[i])
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cell%d.gdo"`, i))
	w.Write(b)
}

func (s *server) handleSave(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	s.mu.Lock()
	err := gdo.WriteSession(&buf, s.farm.Session)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="session.gds"`)
	w.Write(buf.Bytes())
}

// ---- export ----

type option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type aspect struct {
	id, label string
	num, den  int
}

type format struct {
	id, label string
	short     int // the short side: height for landscape, width for portrait
}

var aspects = []aspect{
	{"16:9", "16:9 widescreen", 16, 9},
	{"4:3", "4:3 classic", 4, 3},
	{"3:2", "3:2 photo", 3, 2},
	{"1:1", "1:1 square", 1, 1},
	{"5:4", "5:4", 5, 4},
	{"21:9", "21:9 ultrawide", 64, 27}, // 2560×1080 and its multiples
	{"2.39:1", "2.39:1 cinema", 239, 100},
	{"9:16", "9:16 portrait", 9, 16},
	{"4:5", "4:5 portrait", 4, 5},
	{"2:3", "2:3 portrait", 2, 3},
	{"3:4", "3:4 portrait", 3, 4},
}

var formats = []format{
	{"sd", "SD (480p)", 480},
	{"hd", "HD (720p)", 720},
	{"fhd", "Full HD (1080p)", 1080},
	{"4k", "4K UHD (2160p)", 2160},
	{"8k", "8K UHD (4320p)", 4320},
}

// exportSize is the pixel size of a format at an aspect: the format
// fixes the short side and the long side is rounded to an even number
// (16:9 gives 854×480, 1280×720, 1920×1080, 3840×2160, 7680×4320).
func exportSize(a aspect, f format) (w, h int) {
	long := func(short, num, den int) int {
		return 2 * ((short*num + den) / (2 * den)) // round(short·num/den / 2)·2
	}
	if a.num >= a.den {
		return long(f.short, a.num, a.den), f.short
	}
	return f.short, long(f.short, a.den, a.num)
}

// cover crops the source's centre to w:h and resamples it to w×h:
// box filter when shrinking, bilinear when enlarging.
func cover(f *farm.Frame, w, h int) *farm.Frame {
	cw, ch := float64(f.W), float64(f.H)
	if cw*float64(h) > ch*float64(w) {
		cw = ch * float64(w) / float64(h)
	} else {
		ch = cw * float64(h) / float64(w)
	}
	ox, oy := (float64(f.W)-cw)/2, (float64(f.H)-ch)/2
	sx, sy := cw/float64(w), ch/float64(h) // source pixels per output pixel
	out := &farm.Frame{W: w, H: h, Planes: f.Planes, Channels: f.Channels, Pix: make([]byte, w*h*f.Planes)}
	// Samples are clamped to the crop window, so nothing cropped away
	// bleeds into the edges.
	wx0, wx1 := int(ox), min(int(math.Ceil(ox+cw)), f.W)-1
	wy0, wy1 := int(oy), min(int(math.Ceil(oy+ch)), f.H)-1
	px := func(x, y int) []byte {
		x = min(max(x, wx0), wx1)
		y = min(max(y, wy0), wy1)
		i := (y*f.W + x) * f.Planes
		return f.Pix[i : i+f.Planes]
	}
	parallelRows(h, func(y int) {
		acc := make([]float64, f.Planes)
		for x := 0; x < w; x++ {
			o := out.Pix[(y*w+x)*f.Planes:]
			clear(acc)
			if sx > 1 || sy > 1 { // box: average the footprint
				x0, x1 := ox+float64(x)*sx, ox+float64(x+1)*sx
				y0, y1 := oy+float64(y)*sy, oy+float64(y+1)*sy
				n := 0
				for yy := int(y0); yy < max(int(y1), int(y0)+1); yy++ {
					for xx := int(x0); xx < max(int(x1), int(x0)+1); xx++ {
						for c, v := range px(xx, yy) {
							acc[c] += float64(v)
						}
						n++
					}
				}
				for c := range acc {
					o[c] = byte(acc[c]/float64(n) + 0.5)
				}
				continue
			}
			fx := ox + (float64(x)+0.5)*sx - 0.5
			fy := oy + (float64(y)+0.5)*sy - 0.5
			ix, iy := int(math.Floor(fx)), int(math.Floor(fy))
			ax, ay := fx-float64(ix), fy-float64(iy)
			p00, p10, p01, p11 := px(ix, iy), px(ix+1, iy), px(ix, iy+1), px(ix+1, iy+1)
			for c := range acc {
				top := float64(p00[c])*(1-ax) + float64(p10[c])*ax
				bot := float64(p01[c])*(1-ax) + float64(p11[c])*ax
				o[c] = byte(top*(1-ay) + bot*ay + 0.5)
			}
		}
	})
	return out
}

// parallelRows runs fn for every row in [0, h) on all CPUs.
func parallelRows(h int, fn func(y int)) {
	var wg sync.WaitGroup
	n := runtime.GOMAXPROCS(0)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := i; y < h; y += n {
				fn(y)
			}
		}()
	}
	wg.Wait()
}

func findAspect(id string) (aspect, bool) {
	for _, a := range aspects {
		if a.id == id {
			return a, true
		}
	}
	return aspect{}, false
}

func findFormat(id string) (format, bool) {
	for _, f := range formats {
		if f.id == id {
			return f, true
		}
	}
	return format{}, false
}

// exportSlot serializes exports: an 8K render holds several hundred MB.
var exportSlot = make(chan struct{}, 1)

// handleExport renders a cell at a chosen aspect and format over the
// source cropped to that aspect, and sends it as a PNG download.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i, err := strconv.Atoi(q.Get("cell"))
	a, okA := findAspect(q.Get("aspect"))
	f, okF := findFormat(q.Get("format"))
	if err != nil || i < 0 || i >= gdo.SessionCells || !okA || !okF {
		http.Error(w, "want cell=0-8 and a known aspect and format", http.StatusBadRequest)
		return
	}
	select {
	case exportSlot <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-exportSlot }()
	s.mu.Lock()
	cell, src := s.farm.Session.Cells[i], s.src
	s.mu.Unlock()
	ew, eh := exportSize(a, f)
	start := time.Now()
	img := farm.NewEngine().Render(saved(cell), cover(src, ew, eh), nil).Image()
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("export: cell %d at %dx%d in %v", i, ew, eh, time.Since(start).Round(time.Millisecond))
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ffarm-cell%d-%dx%d.png"`, i, ew, eh))
	w.Write(buf.Bytes())
}

// ---- batch: the selected cell over a folder on the server ----

// batchRequest applies one cell to the PNGs in one server folder,
// replacing each with its render (same name, written atomically), and
// optionally to the PNGs in all its subfolders.
type batchRequest struct {
	Cell      *int   `json:"cell"` // nil = the selected cell
	In        string `json:"in"`
	Recursive bool   `json:"recursive"`
}

// serverPath expands a leading ~ and requires an absolute path: the
// folders are on the server, and its working directory means nothing
// to the person typing.
func serverPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q: give a full path on the server (or start it with ~/)", p)
	}
	return filepath.Clean(p), nil
}

func batchOptions(in string, recursive bool) (batch.Options, error) {
	in, err := serverPath(in)
	if err != nil {
		return batch.Options{}, err
	}
	return batch.Options{In: in, Out: in, InPlace: true, Recursive: recursive}, nil
}

func (s *server) handleBatchStart(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sel := int(s.farm.Session.Selected)
	if req.Cell != nil {
		sel = *req.Cell
	}
	var cell *gdo.File
	if sel >= 0 && sel < gdo.SessionCells {
		cell = s.farm.Session.Cells[sel]
	}
	s.mu.Unlock()
	if cell == nil {
		http.Error(w, "choose a cell (0-8)", http.StatusBadRequest)
		return
	}
	o, err := batchOptions(req.In, req.Recursive)
	if err == nil {
		// The cell as saved, as everything the server renders (see saved).
		err = s.batch.Start(saved(cell), o, fmt.Sprintf("cell %d", sel))
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, batch.ErrBusy) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	log.Printf("batch: cell %d over %s (subfolders: %v), in place", sel, o.In, o.Recursive)
	writeJSON(w, s.batch.Status())
}

func (s *server) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.batch.Status())
}

func (s *server) handleBatchCancel(w http.ResponseWriter, r *http.Request) {
	s.batch.Cancel()
	writeJSON(w, s.batch.Status())
}

// handleBatchList counts the images a batch would read, so the page can
// show it while the folder is being typed.
func (s *server) handleBatchList(w http.ResponseWriter, r *http.Request) {
	o, err := batchOptions(r.URL.Query().Get("in"), r.URL.Query().Get("recursive") == "true")
	var files []string
	if err == nil {
		files, err = batch.List(o)
	}
	res := map[string]any{"count": len(files)}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, res)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func parseDims(s string) (farm.Dims, error) {
	w, h, ok := strings.Cut(strings.ToLower(s), "x")
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if !ok || err1 != nil || err2 != nil || wi < 1 || hi < 1 {
		return farm.Dims{}, fmt.Errorf("size %q: want WIDTHxHEIGHT", s)
	}
	return farm.Dims{W: int32(wi), H: int32(hi)}, nil
}

func loadGrammar(path string) (*farm.Grammar, error) {
	if path == "" {
		return farm.DefaultGrammar(), nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return farm.DefaultGrammar(), nil
	}
	if err != nil {
		return nil, err
	}
	g := &farm.Grammar{}
	if err := json.Unmarshal(data, g); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}

func saveGrammar(path string, g *farm.Grammar) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}
