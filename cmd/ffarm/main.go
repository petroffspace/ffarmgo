// Command ffarm applies a Filter Farm filter to an image, as the
// plugin's final render does. The filter is a .gdo file or one cell of a
// .gds session (by default the session's selected cell, which is what
// the plugin renders).
//
//	ffarm -gdo filter.gdo -in photo.png [-mask selection.png] -out result.png
//	ffarm -gds session.gds [-cell N] -in photo.png -out result.png
//	ffarm -gds session.gds -info
//	ffarm -gds session.gds -in photos/ -out filtered/ [-r] [-overwrite]   # a folder
//
// Evolution, as the Farm dialog does it:
//
//	ffarm -new session.gds [-time T] [-active 3] [-size 131x71]
//	ffarm -breed session.gds -rate 0=10,2=90 -out next.gds [-grammar g.json]
//
// Grayscale sources are filtered in one-channel mode (Photoshop's
// Grayscale mode), CMYK JPEGs in four-channel mode (CMYK mode),
// everything else as RGB. Output is PNG, or TIFF when -out ends in .tif
// or .tiff (the only way to keep a CMYK result as CMYK). A mask image acts as the
// selection: black is unselected, white fully selected, grays partial.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"ffarmgo/internal/batch"
	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
	"ffarmgo/internal/tiffw"
)

func main() {
	gdoPath := flag.String("gdo", "", "filter file (.gdo)")
	gdsPath := flag.String("gds", "", "session file (.gds); renders its selected cell unless -cell is given")
	cell := flag.Int("cell", -1, "session cell 0-8 (default: the session's selected cell)")
	info := flag.Bool("info", false, "print the session's header and cells, then exit")
	in := flag.String("in", "", "source image (PNG, JPEG or GIF)")
	maskPath := flag.String("mask", "", "optional selection mask image (same size as -in)")
	out := flag.String("out", "out.png", "output file (.png, or .tif/.tiff); with a folder -in, the output folder (default: <in>/ffarm-out)")
	recursive := flag.Bool("r", false, "folder -in: include subfolders")
	overwrite := flag.Bool("overwrite", false, "folder -in: replace existing outputs (default: skip them)")
	tiff := flag.Bool("tiff", false, "folder -in: write TIFF instead of PNG")
	maxSpace := flag.Int64("maxspace", 0, "host memory budget in bytes (FilterRecord.maxSpace); small values render in strips like a low-memory host; 0 = one strip")
	var ev evolveOpts
	flag.StringVar(&ev.newPath, "new", "", "create a new session file (the dialog's New Session)")
	flag.StringVar(&ev.breedPath, "breed", "", "breed one generation of this session (the dialog's Breed); writes -out")
	flag.StringVar(&ev.rate, "rate", "", "breed: slider per cell, e.g. 0=10,2=90 (0 = favour, 100 = disfavour, default 50 = neutral)")
	flag.Int64Var(&ev.now, "time", defaultTime(), "new: the time() value that seeds the cells")
	flag.IntVar(&ev.active, "active", -1, "number of active (bred) cells; default 3 for -new, the file's value for -breed")
	flag.IntVar(&ev.sel, "select", -1, "new/breed: mark this cell as the one the filter renders")
	flag.StringVar(&ev.size, "size", "131x71", "new/breed: the dialog's preview cell size, stored in new cells")
	flag.BoolVar(&ev.scalar, "scalar", false, "new/breed: grow scalar-rooted cells (non-RGB image modes)")
	flag.StringVar(&ev.grammar, "grammar", "", "new/breed: load and save the evolving grammar here (port extension; the plugin keeps it in RAM only)")
	flag.Parse()
	if ev.newPath != "" || ev.breedPath != "" {
		ev.out = *out
		if err := runEvolve(ev); err != nil {
			fmt.Fprintln(os.Stderr, "ffarm:", err)
			os.Exit(1)
		}
		return
	}
	if (*gdoPath == "") == (*gdsPath == "") || (*in == "" && !*info) || (*info && *gdsPath == "") {
		flag.Usage()
		os.Exit(2)
	}
	f, err := loadFilter(*gdoPath, *gdsPath, *cell, *info)
	if err == nil && f != nil {
		if st, serr := os.Stat(*in); serr == nil && st.IsDir() {
			err = runFolder(f, *in, *out, *maskPath, *recursive, *overwrite, *tiff, *maxSpace)
		} else {
			err = run(f, *in, *maskPath, *out, *maxSpace)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ffarm:", err)
		os.Exit(1)
	}
}

// loadFilter returns the filter to render: a .gdo, or a cell of a .gds
// session. With info it prints the session instead and returns nil.
func loadFilter(gdoPath, gdsPath string, cell int, info bool) (*gdo.File, error) {
	if gdoPath != "" {
		data, err := os.ReadFile(gdoPath)
		if err != nil {
			return nil, err
		}
		f, err := gdo.Parse(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", gdoPath, err)
		}
		return f, nil
	}
	data, err := os.ReadFile(gdsPath)
	if err != nil {
		return nil, err
	}
	s, err := gdo.ParseSession(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gdsPath, err)
	}
	if info {
		fmt.Printf("session %s: version %d, %d active cells, selected cell %d, directory %q\n",
			gdsPath, s.Tag, s.Active, s.Selected, s.DirString())
		for i, c := range s.Cells {
			mark := " "
			if int32(i) == s.Selected {
				mark = "*"
			}
			fmt.Printf("%s cell %d: offset %6d, root type %d, seed %012X, saved at %dx%d\n",
				mark, i, s.Offsets[i], c.TagB, c.Context.RNG(), c.Context.W, c.Context.H)
		}
		return nil, nil
	}
	if cell < 0 {
		f, err := s.SelectedCell()
		if err != nil {
			return nil, fmt.Errorf("%s: %w (choose one with -cell)", gdsPath, err)
		}
		return f, nil
	}
	if cell >= gdo.SessionCells {
		return nil, fmt.Errorf("-cell %d: a session has cells 0-%d", cell, gdo.SessionCells-1)
	}
	return s.Cells[cell], nil
}

func run(f *gdo.File, in, maskPath, out string, maxSpace int64) error {
	src, err := decode(in)
	if err != nil {
		return err
	}
	frame := farm.FrameFromImage(src)
	var mask []byte
	if maskPath != "" {
		m, err := decode(maskPath)
		if err != nil {
			return err
		}
		if m.Bounds().Dx() != frame.W || m.Bounds().Dy() != frame.H {
			return fmt.Errorf("%s: mask is %dx%d, image is %dx%d", maskPath,
				m.Bounds().Dx(), m.Bounds().Dy(), frame.W, frame.H)
		}
		mask = farm.MaskFromImage(m)
	}
	result := farm.NewEngine().RenderStrips(f, frame, mask, maxSpace)
	o, err := os.Create(out)
	if err != nil {
		return err
	}
	encode := png.Encode
	if ext := strings.ToLower(filepath.Ext(out)); ext == ".tif" || ext == ".tiff" {
		encode = tiffw.Encode
	}
	if err := encode(o, result.Image()); err != nil {
		o.Close()
		return err
	}
	return o.Close()
}

// runFolder applies f to every image in a folder (internal/batch).
func runFolder(f *gdo.File, in, out, maskPath string, recursive, overwrite, tiff bool, maxSpace int64) error {
	if maskPath != "" {
		return errors.New("-mask is for a single image, not a folder")
	}
	outSet := false
	flag.Visit(func(fl *flag.Flag) { outSet = outSet || fl.Name == "out" })
	if !outSet {
		out = filepath.Join(in, "ffarm-out")
	}
	o := batch.Options{In: in, Out: out, Recursive: recursive, Overwrite: overwrite, MaxSpace: maxSpace}
	if tiff {
		o.Format = "tiff"
	}
	last := ""
	p, err := batch.Run(context.Background(), f, o, func(p batch.Progress) {
		if p.Current != "" && p.Current != last {
			last = p.Current
			fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", p.Done+p.Skipped+p.Failed+1, p.Total, p.Current)
		}
	})
	if err != nil {
		return err
	}
	for _, e := range p.Errors {
		fmt.Fprintln(os.Stderr, "ffarm:", e)
	}
	fmt.Fprintf(os.Stderr, "%d rendered, %d skipped (already there), %d failed → %s\n", p.Done, p.Skipped, p.Failed, out)
	if p.Failed > 0 {
		return fmt.Errorf("%d of %d images failed", p.Failed, p.Total)
	}
	return nil
}

func decode(path string) (image.Image, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}
