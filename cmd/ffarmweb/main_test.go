package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ffarmgo/internal/batch"
	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
)

func get(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}

func post(t *testing.T, h http.Handler, path, body string) stateJSON {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body)
	}
	var st stateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Breed through the server matches farm.Breed, and Undo restores the
// session byte for byte.
func TestBreedAndUndo(t *testing.T) {
	t.Parallel()
	const gds = "../../internal/gdo/testdata/render.gds"
	s, err := newServer(options{gds: gds, size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	orig := get(t, h, "/api/session.gds")
	if want, _ := os.ReadFile(gds); !bytes.Equal(orig, want) {
		t.Fatal("loaded session does not save back unchanged")
	}
	post(t, h, "/api/active", `{"active":3}`)
	before := post(t, h, "/api/select", `{"cell":0}`)
	st := post(t, h, "/api/breed", `{"sliders":[0,100,30,50,50,50,50,50,50]}`)
	for i := range st.Cells {
		if changed := st.Cells[i].Rev != before.Cells[i].Rev; changed != (i < 3) {
			t.Errorf("cell %d: changed=%v after breeding 3 active cells", i, changed)
		}
	}

	ref, _ := gdo.ParseSession(orig)
	ref.Active = 3
	f := farm.LoadFarm(ref, farm.DefaultGrammar(), farm.Dims{W: 131, H: 71}, farm.TImage)
	f.Breed([]int{0, 100, 30, 50, 50, 50, 50, 50, 50})
	var want bytes.Buffer
	gdo.WriteSession(&want, f.Session)
	if !bytes.Equal(get(t, h, "/api/session.gds"), want.Bytes()) {
		t.Error("server breed differs from farm.Breed")
	}

	for i := 0; i < gdo.SessionCells; i++ {
		if png := get(t, h, "/api/cell/"+string(rune('0'+i))+".png"); !bytes.HasPrefix(png, []byte("\x89PNG")) {
			t.Fatalf("cell %d preview is not a PNG", i)
		}
	}

	post(t, h, "/api/undo", "")
	got := get(t, h, "/api/session.gds")
	ss, _ := gdo.ParseSession(got)
	ss.Active = 1 // the active count is not part of the undo step's change
	var back bytes.Buffer
	gdo.WriteSession(&back, ss)
	if !bytes.Equal(back.Bytes(), orig) {
		t.Error("undo did not restore the session")
	}
}

func TestBadRequests(t *testing.T) {
	s, err := newServer(options{size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/cell/9.png", ""},
		{"GET", "/api/render/x.png", ""},
		{"POST", "/api/undo", ""},
		{"POST", "/api/active", `{"active":10}`},
		{"POST", "/api/breed", `{"sliders":[101]}`},
		{"POST", "/api/select", `{"cell":9}`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusBadRequest {
			b, _ := io.ReadAll(rec.Body)
			t.Errorf("%s %s: %d %s, want 400", c.method, c.path, rec.Code, b)
		}
	}
}

func TestExportSizes(t *testing.T) {
	want := map[string][2]int{
		"16:9|sd": {854, 480}, "16:9|hd": {1280, 720}, "16:9|fhd": {1920, 1080},
		"16:9|4k": {3840, 2160}, "16:9|8k": {7680, 4320},
		"4:3|sd": {640, 480}, "1:1|4k": {2160, 2160}, "21:9|fhd": {2560, 1080},
		"9:16|fhd": {1080, 1920}, "4:5|hd": {720, 900}, "2.39:1|fhd": {2582, 1080},
	}
	for k, w := range want {
		ai, fi, _ := strings.Cut(k, "|")
		a, _ := findAspect(ai)
		f, _ := findFormat(fi)
		if gw, gh := exportSize(a, f); gw != w[0] || gh != w[1] {
			t.Errorf("%s: %dx%d, want %dx%d", k, gw, gh, w[0], w[1])
		}
	}
}

func TestExport(t *testing.T) {
	t.Parallel()
	s, err := newServer(options{size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	body := get(t, h, "/api/export?cell=2&aspect=9:16&format=sd")
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 480 || b.Dy() != 854 {
		t.Errorf("export is %v, want 480x854", b)
	}
	// The render is the engine's final render over the cropped source.
	want := farm.NewEngine().Render(saved(s.farm.Session.Cells[2]), cover(s.src, 480, 854), nil).Image()
	var wb bytes.Buffer
	(&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&wb, want)
	if !bytes.Equal(body, wb.Bytes()) {
		t.Error("export differs from Engine.Render over the cropped source")
	}
	for _, q := range []string{"cell=9&aspect=16:9&format=sd", "cell=0&aspect=7:5&format=sd", "cell=0&aspect=16:9&format=16k"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/export?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
}

// cover keeps the centre: a frame whose left and right thirds are
// black and middle third white, covered to a square, is all white.
func TestCoverCrops(t *testing.T) {
	f := &farm.Frame{W: 300, H: 100, Planes: 1, Channels: 1, Pix: make([]byte, 300*100)}
	for y := 0; y < 100; y++ {
		for x := 100; x < 200; x++ {
			f.Pix[y*300+x] = 255
		}
	}
	for _, n := range []int{50, 100, 237} { // shrink, same, enlarge
		out := cover(f, n, n)
		for i, v := range out.Pix {
			if v != 255 {
				t.Fatalf("%dx%d: pixel %d = %d, want 255", n, n, i, v)
			}
		}
	}
}

func upload(t *testing.T, h http.Handler, path, name string, data []byte) stateJSON {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body)
	}
	var st stateJSON
	json.Unmarshal(rec.Body.Bytes(), &st)
	return st
}

// A restarted server comes back with the same session, source, sliders
// and generation count, and its grammar has kept what breeding learned:
// breeding after the restart gives what breeding without one gives.
func TestAutosaveRestores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := options{size: "131x71", box: "64x48", state: dir}
	s1, err := newServer(o)
	if err != nil {
		t.Fatal(err)
	}
	h1 := s1.routes()
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var pngData bytes.Buffer
	png.Encode(&pngData, img)
	upload(t, h1, "/api/source", "photo.png", pngData.Bytes())
	post(t, h1, "/api/active", `{"active":4}`)
	for gen := 0; gen < 3; gen++ {
		post(t, h1, "/api/breed", `{"sliders":[0,100,20,80,50,50,50,50,50]}`)
	}
	post(t, h1, "/api/select", `{"cell":2}`)
	before := get(t, h1, "/api/session.gds")
	stBefore := post(t, h1, "/api/active", `{"active":4}`)

	s2, err := newServer(o)
	if err != nil {
		t.Fatal(err)
	}
	h2 := s2.routes()
	if !bytes.Equal(get(t, h2, "/api/session.gds"), before) {
		t.Fatal("restored session differs")
	}
	st := post(t, h2, "/api/active", `{"active":4}`)
	if st.Source != "photo.png" || st.SrcW != 40 || st.SrcH != 30 || st.Breeds != 3 || st.Sliders != stBefore.Sliders {
		t.Errorf("restored %s %dx%d breeds %d, want photo.png 40x30 breeds 3", st.Source, st.SrcW, st.SrcH, st.Breeds)
	}

	for i := 0; i < gdo.SessionCells; i++ {
		p := fmt.Sprintf("/api/cell/%d.png", i)
		if !bytes.Equal(get(t, h1, p), get(t, h2, p)) {
			t.Errorf("cell %d: preview changed across the restart", i)
		}
	}
	// Breeding goes on exactly as without the restart: the grammar and
	// the cells' exact weights (hence their choice logs) were restored.
	for gen := 0; gen < 3; gen++ {
		breed := fmt.Sprintf(`{"sliders":[%d,50,90,30,50,50,50,50,50]}`, 10+30*gen)
		post(t, h1, "/api/breed", breed)
		post(t, h2, "/api/breed", breed)
		if !bytes.Equal(get(t, h1, "/api/session.gds"), get(t, h2, "/api/session.gds")) {
			t.Fatalf("generation %d after the restart differs", gen+1)
		}
	}

	// Explicit flags win over the saved state.
	s3, err := newServer(options{size: "131x71", box: "64x48", state: dir,
		gds: "../../internal/gdo/testdata/render.gds"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := get(t, s3.routes(), "/api/session.gds"), mustRead(t, "../../internal/gdo/testdata/render.gds"); !bytes.Equal(got, want) {
		t.Error("-gds did not take precedence over the saved session")
	}
}

// A damaged state file is ignored, not fatal.
func TestAutosaveDamaged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "session.gds"), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(dir, "source"), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{"), 0o644)
	s, err := newServer(options{size: "131x71", box: "64x48", state: dir})
	if err != nil {
		t.Fatal(err)
	}
	if s.srcName != "built-in gradient" {
		t.Errorf("source %q after a damaged state", s.srcName)
	}
	if _, err := gdo.ParseSession(mustRead(t, filepath.Join(dir, "session.gds"))); err != nil {
		t.Errorf("the damaged session was not replaced: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Loading a .gdo into a cell stores it unchanged and regrows it as
// opening a session holding it would: breeding afterwards matches
// farm.LoadFarm + Breed on that session.
func TestLoadCell(t *testing.T) {
	t.Parallel()
	s, err := newServer(options{gds: "../../internal/gdo/testdata/render.gds", size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	post(t, h, "/api/active", `{"active":6}`)
	gdoData := mustRead(t, "../../internal/farm/testdata/render.gdo")
	before := post(t, h, "/api/active", `{"active":6}`)
	st := upload(t, h, "/api/gdo/5", "render.gdo", gdoData)
	for i := range st.Cells {
		if changed := st.Cells[i].Rev != before.Cells[i].Rev; changed != (i == 5) {
			t.Errorf("cell %d: changed=%v after loading into cell 5", i, changed)
		}
	}
	if got := get(t, h, "/api/gdo/5"); !bytes.Equal(got, gdoData) {
		t.Error("the loaded cell does not download back unchanged")
	}

	ref, _ := gdo.ParseSession(get(t, h, "/api/session.gds"))
	f := farm.LoadFarm(ref, farm.DefaultGrammar(), farm.Dims{W: 131, H: 71}, farm.TImage)
	sliders := []int{20, 50, 80, 50, 50, 0, 50, 50, 50}
	f.Breed(sliders)
	var want bytes.Buffer
	gdo.WriteSession(&want, f.Session)
	post(t, h, "/api/breed", `{"sliders":[20,50,80,50,50,0,50,50,50]}`)
	if !bytes.Equal(get(t, h, "/api/session.gds"), want.Bytes()) {
		t.Error("breeding after loading a cell differs from LoadFarm + Breed")
	}

	post(t, h, "/api/undo", "") // undo the breed
	post(t, h, "/api/undo", "") // undo the load
	orig := mustRead(t, "../../internal/gdo/testdata/render.gds")
	ss, _ := gdo.ParseSession(get(t, h, "/api/session.gds"))
	ss.Active = 1
	var back bytes.Buffer
	gdo.WriteSession(&back, ss)
	if !bytes.Equal(back.Bytes(), orig) {
		t.Error("undo did not remove the loaded cell")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/gdo/3", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("load without a file: %d, want 400", rec.Code)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "junk.gdo")
	fw.Write([]byte("not a filter"))
	mw.Close()
	req = httptest.NewRequest("POST", "/api/gdo/3", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("load of a junk file: %d, want 400", rec.Code)
	}
}

// Cells bred in memory carry float32 weights; what the server renders
// is the cell as the saved session holds it, as the plugin's final
// render and `ffarm -gds` read it.
func TestRenderUsesSavedCell(t *testing.T) {
	t.Parallel()
	s, err := newServer(options{size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	s.setFrame(scale(defaultSource(), 160, 90), "small")
	h := s.routes()
	post(t, h, "/api/active", `{"active":9}`)
	post(t, h, "/api/breed", `{"sliders":[0,10,20,30,40,60,70,90,100]}`)
	ss, err := gdo.ParseSession(get(t, h, "/api/session.gds"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < gdo.SessionCells; i++ {
		got := get(t, h, fmt.Sprintf("/api/render/%d.png", i))
		var want bytes.Buffer
		png.Encode(&want, farm.NewEngine().Render(ss.Cells[i], s.src, nil).Image())
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("cell %d: render differs from the saved session's cell", i)
		}
	}
}

// Undo restores the exact farm: breeding again with the same sliders
// gives the same generation as the first time.
func TestUndoExact(t *testing.T) {
	t.Parallel()
	s, err := newServer(options{size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	post(t, h, "/api/active", `{"active":9}`)
	for gen := 0; gen < 4; gen++ {
		breed := fmt.Sprintf(`{"sliders":[%d,10,20,30,40,60,70,90,100]}`, gen*25)
		post(t, h, "/api/breed", breed)
		first := get(t, h, "/api/session.gds")
		g1, _ := json.Marshal(s.farm.Grammar)
		post(t, h, "/api/undo", "")
		st := post(t, h, "/api/breed", breed)
		g2, _ := json.Marshal(s.farm.Grammar)
		if !bytes.Equal(get(t, h, "/api/session.gds"), first) || !bytes.Equal(g1, g2) {
			t.Fatalf("generation %d: breeding after undo differs", gen+1)
		}
		if st.Breeds != gen+1 {
			t.Fatalf("generation counter %d, want %d", st.Breeds, gen+1)
		}
	}
}

// The batch replaces each image in a server folder with the chosen
// cell's render, the cell as saved.
func TestBatch(t *testing.T) {
	t.Parallel()
	s, err := newServer(options{size: "131x71", box: "64x48"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 30, 20))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 11)
	}
	var pngData bytes.Buffer
	png.Encode(&pngData, img)
	for _, n := range []string{"x.png", "y.png"} {
		os.WriteFile(filepath.Join(dir, n), pngData.Bytes(), 0o644)
	}

	try := func(body string, code int) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/batch", strings.NewReader(body)))
		if rec.Code != code {
			t.Fatalf("POST /api/batch %s: %d %s, want %d", body, rec.Code, rec.Body, code)
		}
	}
	try(`{"in":"`+dir+`"}`, http.StatusBadRequest) // no cell and nothing selected
	try(`{"cell":9,"in":"`+dir+`"}`, http.StatusBadRequest)
	try(`{"cell":3,"in":"relative/dir"}`, http.StatusBadRequest)
	try(`{"cell":3,"in":"`+filepath.Join(dir, "missing")+`"}`, http.StatusBadRequest)
	post(t, h, "/api/active", `{"active":9}`)
	post(t, h, "/api/breed", `{"sliders":[0,10,20,30,40,60,70,90,100]}`) // float weights in memory
	post(t, h, "/api/select", `{"cell":1}`)

	var list struct {
		Count int `json:"count"`
	}
	json.Unmarshal(get(t, h, "/api/batch/list?in="+dir), &list)
	if list.Count != 2 {
		t.Errorf("list: %+v, want 2 images", list)
	}

	try(`{"cell":3,"in":"`+dir+`"}`, http.StatusOK) // the chosen cell, not the selected one
	s.batch.Wait()
	var st batch.Status
	json.Unmarshal(get(t, h, "/api/batch"), &st)
	if st.Running || st.Done != 2 || st.Error != "" || st.Label != "cell 3" {
		t.Fatalf("status %+v", st)
	}
	ss, _ := gdo.ParseSession(get(t, h, "/api/session.gds"))
	var want bytes.Buffer
	png.Encode(&want, farm.NewEngine().Render(ss.Cells[3], farm.FrameFromImage(img), nil).Image())
	for _, n := range []string{"x.png", "y.png"} {
		if got := mustRead(t, filepath.Join(dir, n)); !bytes.Equal(got, want.Bytes()) {
			t.Errorf("%s: not replaced by cell 3's render", n)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("folder has %d entries, want just the 2 images", len(entries))
	}

	// Subfolders only when asked.
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "z.png"), pngData.Bytes(), 0o644)
	json.Unmarshal(get(t, h, "/api/batch/list?in="+dir), &list)
	n1 := list.Count
	json.Unmarshal(get(t, h, "/api/batch/list?recursive=true&in="+dir), &list)
	if n1 != 2 || list.Count != 3 {
		t.Errorf("list: %d without subfolders, %d with; want 2 and 3", n1, list.Count)
	}
	try(`{"cell":3,"in":"`+dir+`"}`, http.StatusOK)
	s.batch.Wait()
	if got := mustRead(t, filepath.Join(dir, "sub", "z.png")); !bytes.Equal(got, pngData.Bytes()) {
		t.Error("sub/z.png was changed without include-subfolders")
	}
	try(`{"cell":3,"in":"`+dir+`","recursive":true}`, http.StatusOK)
	s.batch.Wait()
	if got := mustRead(t, filepath.Join(dir, "sub", "z.png")); !bytes.Equal(got, want.Bytes()) {
		t.Error("sub/z.png was not replaced with include-subfolders")
	}
}
