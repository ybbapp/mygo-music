package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/egoist/mygo/ui"
	"mygo-music/internal/music"
)

// Restarting resumes queued downloads with their source and quality, without overwriting files.
func TestDownloadsRestore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	root := t.TempDir()
	dir := filepath.Join(root, "music")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "downloads.json")
	explicit := filepath.Join(dir, "Chosen.mp3")
	if err := os.WriteFile(explicit, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	first := song("1", "First")
	first.Source = "other"
	second := song("2", "Second")
	second.Source = "fake"
	done := song("3", "Done")
	done.Source = "fake"
	failed := song("4", "Failed")
	failed.Source = "fake"
	missing := song("5", "Missing")
	missing.Source = "removed"
	location := song("6", "No destination")
	location.Source = "fake"
	saved := []savedDownload{
		{Track: first, Dir: dir, Quality: "lossless", State: "queued"},
		{Track: second, Dir: dir, Path: explicit, Quality: "standard", State: "downloading"},
		{Track: done, Dir: dir, State: "done"},
		{Track: failed, Dir: dir, State: "timeout", Attempts: 1},
		{Track: missing, Dir: dir, State: "queued"},
		{Track: location, State: "downloading"},
	}
	if err := writeDownloads(path, saved); err != nil {
		t.Fatal(err)
	}
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadConcurrency = 1
	var firstCalls, secondCalls atomic.Int32
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		secondCalls.Add(1)
		if id != "2" || quality != "standard" {
			t.Errorf("wrong fake request %s %s", id, quality)
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	other := newFakeSource("other")
	other.stream = func(id, quality string) (*music.Stream, error) {
		firstCalls.Add(1)
		if id != "1" || quality != "lossless" {
			t.Errorf("wrong other request %s %s", id, quality)
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	addFake(a, other)
	a.startDownloadStore(path)
	until(t, ch, func() bool { return a.downloads.loaded && len(a.downloads.batches) == 0 })
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls %d, %d", firstCalls.Load(), secondCalls.Load())
	}
	if len(a.downloads.history) != 6 {
		t.Fatalf("history length %d", len(a.downloads.history))
	}
	for i, want := range []string{"done", "done", "done", "timeout", "failed", "failed"} {
		if got := a.downloads.history[i].state; got != want {
			t.Fatalf("job %d: %s, want %s", i, got, want)
		}
	}
	if !errors.Is(a.downloads.history[3].err, context.DeadlineExceeded) || !errors.Is(a.downloads.history[4].err, music.ErrUnavailable) || !errors.Is(a.downloads.history[5].err, errDownloadNeedsLocation) {
		t.Fatal("restored failure reasons are wrong")
	}
	for name, want := range map[string]string{"First.mp3": "audio", "Chosen.mp3": "existing", "Chosen (1).mp3": "audio"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
	}
	a.flushDownloads()
	b := newTestApp()
	bch := posts(b)
	b.startDownloadStore(path)
	until(t, bch, func() bool { return b.downloads.loaded })
	if len(b.downloads.history) != 6 || b.downloads.history[0].state != "done" || b.downloads.history[3].state != "timeout" || len(b.downloads.batches) != 0 {
		t.Fatal("completed and failed tasks did not survive a second restart")
	}
	b.flushDownloads()
}

// Closing while a download is active persists its pending state for the next launch.
func TestDownloadStoreFlushPending(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "downloads.json")
	a := newTestApp()
	ch := posts(a)
	a.startDownloadStore(path)
	until(t, ch, func() bool { return a.downloads.loaded })
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake(a).stream = func(string, string) (*music.Stream, error) {
		started <- struct{}{}
		<-release
		return nil, music.ErrUnavailable
	}
	a.settings.DownloadFolder = root
	a.download(song("1", "Pending"))
	wait(t, started, "download to start")
	a.flushDownloads()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved []savedDownload
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Track.Source != "fake" || saved[0].State != "downloading" {
		t.Fatalf("saved %+v", saved)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	b := newTestApp()
	bch := posts(b)
	fake(b).stream = func(string, string) (*music.Stream, error) { return &music.Stream{URL: srv.URL, Type: "mp3"}, nil }
	b.startDownloadStore(path)
	until(t, bch, func() bool { return b.downloads.loaded && len(b.downloads.batches) == 0 })
	if len(b.downloads.history) != 1 || b.downloads.history[0].state != "done" {
		t.Fatal("pending download did not resume")
	}
	b.flushDownloads()
	close(release)
	runPost(t, ch)
}

// Clearing completed downloads is kept across restart without removing failed tasks.
func TestDownloadStoreClearCompleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downloads.json")
	tr := song("1", "Complete")
	tr.Source = "fake"
	failed := song("2", "Failed")
	failed.Source = "fake"
	if err := writeDownloads(path, []savedDownload{{Track: tr, State: "done"}, {Track: failed, State: "failed", Problem: "signIn"}}); err != nil {
		t.Fatal(err)
	}
	a := newTestApp()
	ch := posts(a)
	a.startDownloadStore(path)
	until(t, ch, func() bool { return a.downloads.loaded })
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("Downloads"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Clear Completed"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	a.flushDownloads()
	b := newTestApp()
	bch := posts(b)
	b.startDownloadStore(path)
	until(t, bch, func() bool { return b.downloads.loaded })
	if len(b.downloads.history) != 1 || b.downloads.history[0].track.ID != "2" || !errors.Is(b.downloads.history[0].err, music.ErrSignIn) {
		t.Fatal("failed task was not preserved")
	}
	b.flushDownloads()
}

// Loading a damaged download list leaves the original file untouched and reports the problem.
func TestDownloadStoreDamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downloads.json")
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newTestApp()
	ch := posts(a)
	a.startDownloadStore(path)
	until(t, ch, func() bool { return a.downloads.store == nil })
	if a.toast != "Couldn't load the download list" {
		t.Fatalf("toast %q", a.toast)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "invalid" {
		t.Fatalf("changed original: %q, %v", b, err)
	}
}

// A restored task whose source was disabled can be retried after the source returns.
func TestDownloadRestoreSourceReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	root := t.TempDir()
	path := filepath.Join(root, "downloads.json")
	tr := song("1", "Returns")
	tr.Source = "other"
	if err := writeDownloads(path, []savedDownload{{Track: tr, Dir: root, Quality: "high", State: "queued"}}); err != nil {
		t.Fatal(err)
	}
	a := newTestApp()
	ch := posts(a)
	other := newFakeSource("other")
	other.stream = func(string, string) (*music.Stream, error) { return &music.Stream{URL: srv.URL, Type: "mp3"}, nil }
	lib := addFake(a, other)
	a.libs = a.libs[:1]
	a.startDownloadStore(path)
	until(t, ch, func() bool { return a.downloads.loaded })
	job := a.downloads.history[0]
	if job.state != "failed" || !errors.Is(job.err, music.ErrUnavailable) {
		t.Fatal("missing source did not leave a retryable failure")
	}
	a.libs = append(a.libs, lib)
	a.retryDownload(job)
	until(t, ch, func() bool { return job.state == "done" })
	if b, err := os.ReadFile(filepath.Join(root, "Returns.mp3")); err != nil || string(b) != "audio" {
		t.Fatalf("saved %q: %v", b, err)
	}
	a.flushDownloads()
}
