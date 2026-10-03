package batch

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
)

func filter(t *testing.T) *gdo.File {
	t.Helper()
	data, err := os.ReadFile("../farm/testdata/render.gdo")
	if err != nil {
		t.Fatal(err)
	}
	f, err := gdo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeImage(t *testing.T, path string, img image.Image) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var err error
	if filepath.Ext(path) == ".jpg" {
		err = jpeg.Encode(&buf, img, nil)
	} else {
		err = png.Encode(&buf, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// folder makes: a.png (RGB), b.jpg, c.png (gray), notes.txt, sub/d.png.
func folder(t *testing.T) string {
	dir := t.TempDir()
	rgb := image.NewRGBA(image.Rect(0, 0, 40, 30))
	gray := image.NewGray(image.Rect(0, 0, 33, 21))
	for i := range rgb.Pix {
		rgb.Pix[i] = byte(i * 5)
	}
	for i := range gray.Pix {
		gray.Pix[i] = byte(i * 3)
	}
	writeImage(t, filepath.Join(dir, "a.png"), rgb)
	writeImage(t, filepath.Join(dir, "b.jpg"), rgb)
	writeImage(t, filepath.Join(dir, "c.png"), gray)
	writeImage(t, filepath.Join(dir, "sub", "d.png"), rgb)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o644)
	return dir
}

// want is the CLI's render of one image.
func want(t *testing.T, f *gdo.File, path string) []byte {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	png.Encode(&buf, farm.NewEngine().Render(f, farm.FrameFromImage(img), nil).Image())
	return buf.Bytes()
}

func TestRun(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := folder(t)
	out := filepath.Join(dir, "out")
	o := Options{In: dir, Out: out}
	var reports int
	p, err := Run(context.Background(), f, o, func(Progress) { reports++ })
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.Done != 3 || p.Failed != 0 || p.Skipped != 0 {
		t.Fatalf("progress %+v, want 3 of 3 done", p)
	}
	if reports < 4 {
		t.Errorf("%d progress reports, want one per image and a first one", reports)
	}
	for _, name := range []string{"a", "b", "c"} {
		in := filepath.Join(dir, name+".png")
		if name == "b" {
			in = filepath.Join(dir, "b.jpg")
		}
		got, err := os.ReadFile(filepath.Join(out, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want(t, f, in)) {
			t.Errorf("%s: output differs from a single render", name)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "sub")); err == nil {
		t.Error("a non-recursive batch went into sub/")
	}

	// A second run skips what is there; Overwrite renders again.
	p, _ = Run(context.Background(), f, o, nil)
	if p.Skipped != 3 || p.Done != 0 {
		t.Errorf("rerun: %+v, want 3 skipped", p)
	}
	o.Overwrite = true
	p, _ = Run(context.Background(), f, o, nil)
	if p.Done != 3 {
		t.Errorf("overwrite: %+v, want 3 done", p)
	}
}

func TestRecursiveAndOutputInside(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := folder(t)
	out := filepath.Join(dir, "ffarm-out")
	o := Options{In: dir, Out: out, Recursive: true, Format: "tiff"}
	if _, err := Run(context.Background(), f, o, nil); err != nil {
		t.Fatal(err)
	}
	// The output folder sits inside the input folder: a second listing
	// must not pick up the first run's outputs.
	files, _ := List(o)
	if !slices.Equal(files, []string{"a.png", "b.jpg", "c.png", filepath.Join("sub", "d.png")}) {
		t.Errorf("listing %v", files)
	}
	for _, name := range []string{"a.tif", "b.tif", "c.tif", filepath.Join("sub", "d.tif")} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing output %s", name)
		}
	}
}

func TestFailuresAndErrors(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := folder(t)
	os.WriteFile(filepath.Join(dir, "broken.png"), []byte("not a png"), 0o644)
	p, err := Run(context.Background(), f, Options{In: dir, Out: filepath.Join(dir, "o")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Failed != 1 || p.Done != 3 || len(p.Errors) != 1 {
		t.Errorf("progress %+v, want 1 failure and 3 done", p)
	}
	if _, err := Run(context.Background(), f, Options{In: filepath.Join(dir, "missing"), Out: dir}, nil); err == nil {
		t.Error("a missing input folder was accepted")
	}
	if _, err := Run(context.Background(), f, Options{In: dir, Out: dir, Format: "bmp"}, nil); err == nil {
		t.Error("an unknown format was accepted")
	}
}

func TestRunnerCancel(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.White)
	for i := 0; i < 40; i++ {
		writeImage(t, filepath.Join(dir, "img"+string(rune('a'+i%26))+string(rune('a'+i/26))+".png"), img)
	}
	var r Runner
	if err := r.Start(f, Options{In: dir, Out: filepath.Join(dir, "o")}, "cell 0"); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(f, Options{In: dir, Out: dir}, "again"); err != ErrBusy {
		t.Errorf("second Start: %v, want ErrBusy", err)
	}
	r.Cancel()
	r.Wait()
	st := r.Status()
	if st.Running || st.Error != "cancelled" || st.Done+st.Skipped >= st.Total {
		t.Errorf("after cancel: %+v", st)
	}
	if st.Label != "cell 0" || st.Finished.IsZero() {
		t.Errorf("status %+v", st)
	}
	if err := r.Start(f, Options{In: filepath.Join(dir, "nope"), Out: dir}, "x"); err == nil {
		t.Error("Start accepted a missing folder")
	}
}

// In place: every PNG is replaced by its render; nothing else in the
// folder is read, written or added.
func TestInPlace(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := folder(t) // a.png, b.jpg, c.png, notes.txt, sub/d.png
	os.Chmod(filepath.Join(dir, "c.png"), 0o640)
	jpgBefore, _ := os.ReadFile(filepath.Join(dir, "b.jpg"))
	before, _ := os.ReadDir(dir)
	wantA := want(t, f, filepath.Join(dir, "a.png"))
	wantC := want(t, f, filepath.Join(dir, "c.png"))

	p, err := Run(context.Background(), f, Options{In: dir, InPlace: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 2 || p.Done != 2 || p.Failed != 0 {
		t.Fatalf("progress %+v, want the 2 PNGs done", p)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.png")); !bytes.Equal(got, wantA) {
		t.Error("a.png was not replaced by its render")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "c.png")); !bytes.Equal(got, wantC) {
		t.Error("c.png was not replaced by its render")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.jpg")); !bytes.Equal(got, jpgBefore) {
		t.Error("b.jpg was touched")
	}
	if st, _ := os.Stat(filepath.Join(dir, "c.png")); st.Mode().Perm() != 0o640 {
		t.Errorf("c.png mode %v, want 0640 kept", st.Mode().Perm())
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Errorf("folder had %d entries, now %d", len(before), len(after))
	}
}

// In place with subfolders: the PNGs of every subfolder are replaced
// too, under their own names; nothing is added anywhere.
func TestInPlaceRecursive(t *testing.T) {
	t.Parallel()
	f := filter(t)
	dir := folder(t) // a.png, b.jpg, c.png, notes.txt, sub/d.png
	wantD := want(t, f, filepath.Join(dir, "sub", "d.png"))
	p, err := Run(context.Background(), f, Options{In: dir, InPlace: true, Recursive: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.Done != 3 {
		t.Fatalf("progress %+v, want a.png, c.png and sub/d.png done", p)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sub", "d.png")); !bytes.Equal(got, wantD) {
		t.Error("sub/d.png was not replaced by its render")
	}
	if sub, _ := os.ReadDir(filepath.Join(dir, "sub")); len(sub) != 1 {
		t.Errorf("sub/ has %d entries, want 1", len(sub))
	}
}
