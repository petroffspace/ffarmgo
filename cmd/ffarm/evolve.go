package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ffarmgo/internal/farm"
	"ffarmgo/internal/gdo"
)

// evolveOpts are the Farm dialog operations: New Session and Breed.
type evolveOpts struct {
	newPath, breedPath, out string
	rate                    string
	now                     int64
	active, sel             int
	size                    string
	scalar                  bool
	grammar                 string
}

// runEvolve creates (-new) or breeds (-breed) a session and writes it.
func runEvolve(o evolveOpts) error {
	d, err := parseSize(o.size)
	if err != nil {
		return err
	}
	typ := farm.TImage
	if o.scalar {
		typ = farm.TScalar
	}
	g, err := loadGrammar(o.grammar)
	if err != nil {
		return err
	}
	var f *farm.Farm
	var dst string
	if o.newPath != "" {
		active := o.active
		if active < 0 {
			active = 3 // DAT_1000a010 when the dialog starts
		}
		f = farm.NewSession(g, typ, int32(active), uint32(o.now), d)
		dst = o.newPath
	} else {
		data, err := os.ReadFile(o.breedPath)
		if err != nil {
			return err
		}
		s, err := gdo.ParseSession(data)
		if err != nil {
			return fmt.Errorf("%s: %w", o.breedPath, err)
		}
		if o.active >= 0 {
			s.Active = int32(o.active)
		}
		sliders, err := parseRatings(o.rate)
		if err != nil {
			return err
		}
		f = farm.LoadFarm(s, g, d, typ)
		f.Breed(sliders)
		dst = o.out
		if !strings.HasSuffix(strings.ToLower(dst), ".gds") {
			return errors.New("-breed needs -out with a .gds file name")
		}
	}
	if o.sel >= 0 {
		if o.sel >= gdo.SessionCells {
			return fmt.Errorf("-select %d: a session has cells 0-%d", o.sel, gdo.SessionCells-1)
		}
		f.Session.Selected = int32(o.sel)
	}
	var buf bytes.Buffer
	if err := gdo.WriteSession(&buf, f.Session); err != nil {
		return err
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return saveGrammar(o.grammar, f.Grammar)
}

// parseRatings reads "cell=slider,…" (sliders 0..100; unrated cells 50).
// The dialog's slider is inverted: 0 favours the cell's choices, 100
// disfavours them, 50 leaves them alone.
func parseRatings(s string) ([]int, error) {
	sliders := make([]int, gdo.SessionCells)
	for i := range sliders {
		sliders[i] = 50
	}
	if s == "" {
		return sliders, nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		cell, err1 := strconv.Atoi(k)
		val, err2 := strconv.Atoi(v)
		if !ok || err1 != nil || err2 != nil || cell < 0 || cell >= gdo.SessionCells || val < 0 || val > 100 {
			return nil, fmt.Errorf("-rate %q: want cell=slider pairs, cells 0-8, sliders 0-100", kv)
		}
		sliders[cell] = val
	}
	return sliders, nil
}

func parseSize(s string) (farm.Dims, error) {
	w, h, ok := strings.Cut(strings.ToLower(s), "x")
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if !ok || err1 != nil || err2 != nil || wi < 0 || hi < 0 {
		return farm.Dims{}, fmt.Errorf("-size %q: want WIDTHxHEIGHT", s)
	}
	return farm.Dims{W: int32(wi), H: int32(hi)}, nil
}

// The plugin keeps its evolving grammar only in RAM, for one Photoshop
// session. -grammar persists it between runs of this tool (a port
// extension, not a Filter Farm file format); without it every run starts
// from the grammar the plugin builds at load.
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
	return os.WriteFile(path, data, 0o644)
}

func defaultTime() int64 { return time.Now().Unix() }
