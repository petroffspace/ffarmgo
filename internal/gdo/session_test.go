package gdo

import (
	"bytes"
	"os"
	"testing"
)

func loadSession(t *testing.T) ([]byte, *Session) {
	t.Helper()
	data, err := os.ReadFile("testdata/render.gds")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSession(data)
	if err != nil {
		t.Fatalf("ParseSession: %v", err)
	}
	return data, s
}

func TestSessionHeader(t *testing.T) {
	_, s := loadSession(t)
	if s.Tag != 1000 || s.Active != 1 || s.Selected != 0 {
		t.Fatalf("header: tag %d active %d selected %d", s.Tag, s.Active, s.Selected)
	}
	if s.DirString() != `C:\Users\Alex\Desktop` {
		t.Fatalf("dir = %q", s.DirString())
	}
	want := [SessionCells]uint32{174, 8076, 15978, 23880, 31782, 39684, 47586, 55488, 63390}
	if s.Offsets != want {
		t.Fatalf("offsets %v", s.Offsets)
	}
}

// render.gdo is cell 0 of render.gds, byte for byte.
func TestSessionCellIsGdo(t *testing.T) {
	_, s := loadSession(t)
	gdo, err := os.ReadFile("testdata/render.gdo")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.SelectedCell()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Serialize(c)
	if err != nil || !bytes.Equal(b, gdo) {
		t.Fatalf("selected cell does not serialize to render.gdo (%v)", err)
	}
}

// Writing reproduces the file exactly, including the stale bytes after
// the directory's NUL.
func TestSessionRoundTrip(t *testing.T) {
	data, s := loadSession(t)
	var buf bytes.Buffer
	if err := WriteSession(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("round trip differs (%d vs %d bytes)", buf.Len(), len(data))
	}
}

func TestSessionNoSelection(t *testing.T) {
	_, s := loadSession(t)
	s.Selected = -1
	if _, err := s.SelectedCell(); err == nil {
		t.Fatal("expected an error for a session with no selected cell")
	}
	var buf bytes.Buffer
	if err := WriteSession(&buf, s); err != nil {
		t.Fatal(err)
	}
	back, err := ParseSession(buf.Bytes())
	if err != nil || back.Selected != -1 {
		t.Fatalf("selected = %d after round trip (%v)", back.Selected, err)
	}
}

func TestSessionTruncated(t *testing.T) {
	data, _ := loadSession(t)
	if _, err := ParseSession(data[:100]); err == nil {
		t.Fatal("short header accepted")
	}
	if _, err := ParseSession(data[:200]); err == nil {
		t.Fatal("missing cells accepted")
	}
}
