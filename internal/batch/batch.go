// Package batch applies one filter to every image in a folder. Each
// image gets exactly the render `ffarm -gdo … -in image` gives it: its
// own size and colour mode, a fresh engine (one plugin invocation per
// image), no selection.
package batch

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // decoders
	_ "image/jpeg" // decoders
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
	"ffarmgo/internal/tiffw"
)

// Options describe a batch.
type Options struct {
	In, Out   string // folders; Out is created as needed
	Recursive bool   // also process subfolders, mirroring them under Out
	Format    string // "png" (default) or "tiff"
	Overwrite bool   // replace existing outputs instead of skipping them
	InPlace   bool   // replace each PNG with its result; other files are not read (Out, Format unused)
	MaxSpace  int64  // host memory budget per render (see farm.StripRows); 0 = one strip
}

// Progress is a batch's state, reported after every image.
type Progress struct {
	Total   int      `json:"total"`
	Done    int      `json:"done"`    // rendered and written
	Skipped int      `json:"skipped"` // output already there
	Failed  int      `json:"failed"`
	Current string   `json:"current"` // the image being rendered, relative to In
	Errors  []string `json:"errors"`  // the first few failures
}

const maxErrors = 20

// Inputs reports whether a file name is an image the batch reads.
func Inputs(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	}
	return false
}

// List returns the images a batch would process, as paths relative to
// o.In, sorted. The output folder is never read as input.
func List(o Options) ([]string, error) {
	in, err := filepath.Abs(o.In)
	if err != nil {
		return nil, err
	}
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(in)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", o.In)
	}
	var files []string
	err = filepath.WalkDir(in, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != in && (!o.Recursive || path == out) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && Inputs(d.Name()) && (!o.InPlace || strings.EqualFold(filepath.Ext(d.Name()), ".png")) {
			rel, err := filepath.Rel(in, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	slices.Sort(files)
	return files, err
}

// OutputName is the output path of an input image (relative to In).
func OutputName(o Options, rel string) string {
	if o.InPlace {
		return filepath.Join(o.In, rel)
	}
	ext := ".png"
	if o.Format == "tiff" {
		ext = ".tif"
	}
	return filepath.Join(o.Out, strings.TrimSuffix(rel, filepath.Ext(rel))+ext)
}

// Run applies f to every image of the batch, calling report after each
// one (and once before the first). It stops between images when ctx is
// cancelled and returns ctx's error. Failures of single images are
// counted and listed in the progress, not returned.
func Run(ctx context.Context, f *gdo.File, o Options, report func(Progress)) (Progress, error) {
	var p Progress
	if o.Format != "" && o.Format != "png" && o.Format != "tiff" {
		return p, fmt.Errorf("unknown output format %q (png or tiff)", o.Format)
	}
	files, err := List(o)
	if err != nil {
		return p, err
	}
	p.Total = len(files)
	if report == nil {
		report = func(Progress) {}
	}
	report(p)
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			p.Current = ""
			report(p)
			return p, err
		}
		out := OutputName(o, rel)
		if !o.Overwrite && !o.InPlace {
			if _, err := os.Stat(out); err == nil {
				p.Skipped++
				report(p)
				continue
			}
		}
		p.Current = rel
		report(p)
		if err := One(f, filepath.Join(o.In, rel), out, o.MaxSpace); err != nil {
			p.Failed++
			if len(p.Errors) < maxErrors {
				p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", rel, err))
			}
		} else {
			p.Done++
		}
	}
	p.Current = ""
	report(p)
	return p, nil
}

// One renders a single image file through f and writes the result to
// out (PNG, or TIFF for a .tif/.tiff name), replacing it atomically and
// keeping an existing file's permissions. out may be in itself.
func One(f *gdo.File, in, out string, maxSpace int64) error {
	fh, err := os.Open(in)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(fh)
	fh.Close()
	if err != nil {
		return err
	}
	result := farm.NewEngine().RenderStrips(f, farm.FrameFromImage(img), nil, maxSpace)
	encode := png.Encode
	if ext := strings.ToLower(filepath.Ext(out)); ext == ".tif" || ext == ".tiff" {
		encode = tiffw.Encode
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(out); err == nil {
		mode = st.Mode().Perm()
	}
	return writeAtomic(out, mode, func(w io.Writer) error { return encode(w, result.Image()) })
}

func writeAtomic(path string, mode os.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	err = write(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp makes 0600 files.
		err = os.Chmod(tmp.Name(), mode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// ErrBusy is returned by a Runner asked to start while a batch runs.
var ErrBusy = errors.New("a batch is already running")

// Status is a Runner's view of its current or last batch.
type Status struct {
	Progress
	Running  bool      `json:"running"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	In       string    `json:"in"`
	Out      string    `json:"out"`
	Label    string    `json:"label"` // what the filter is, e.g. "cell 4"
	Error    string    `json:"error"` // why the batch stopped early, if it did
}

// Runner runs one batch at a time in the background.
type Runner struct {
	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
	done   chan struct{}
}

// Start begins a batch with filter f; label names the filter in Status.
// The batch's file list is checked before Start returns, so a missing
// folder is reported at once.
func (r *Runner) Start(f *gdo.File, o Options, label string) error {
	if _, err := List(o); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Running {
		return ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	r.st = Status{Running: true, Started: time.Now(), In: o.In, Out: o.Out, Label: label}
	go func() {
		defer close(r.done)
		_, err := Run(ctx, f, o, func(p Progress) {
			r.mu.Lock()
			r.st.Progress = p
			r.st.Progress.Errors = slices.Clone(p.Errors)
			r.mu.Unlock()
		})
		r.mu.Lock()
		r.st.Running, r.st.Finished = false, time.Now()
		if errors.Is(err, context.Canceled) {
			r.st.Error = "cancelled"
		} else if err != nil {
			r.st.Error = err.Error()
		}
		r.mu.Unlock()
		cancel()
	}()
	return nil
}

// Status returns a copy of the current or last batch's state.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st
	s.Errors = slices.Clone(s.Errors)
	return s
}

// Cancel stops the running batch after its current image.
func (r *Runner) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// Wait blocks until the running batch (if any) has finished.
func (r *Runner) Wait() {
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}
