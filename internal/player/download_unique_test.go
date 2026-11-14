package player

import (
	"os"
	"path/filepath"
	"testing"
)

// Saving a song leaves an existing file intact and chooses another name.
func TestSaveUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.mp3")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Song{file: f}).SaveUnique(dir, "song.mp3"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"song.mp3": "original", "song (1).mp3": "new"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
	}
}

// Saving on a filesystem without hard links still preserves existing files.
func TestSaveUniqueWithoutLinks(t *testing.T) {
	real := linkSong
	linkSong = func(string, string) error { return os.ErrPermission }
	defer func() { linkSong = real }()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "song.mp3"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Song{file: f}).SaveUnique(dir, "song.mp3"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"song.mp3": "old", "song (1).mp3": "new"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
	}
}
