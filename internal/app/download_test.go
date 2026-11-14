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
	"mygo-music/internal/local"
	"mygo-music/internal/music"
)

// Downloading a playlist continues after a failed song and preserves existing files.
func TestDownloadPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("audio"))
	}))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	dir := t.TempDir()
	a.settings.DownloadFolder = dir
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		return &music.Stream{URL: srv.URL + "/" + id, Type: "mp3"}, nil
	}
	tracks := []music.Track{song("one", "First", "Artist"), song("bad", "Bad", "Artist"), song("two", "Last", "Artist")}
	if err := os.WriteFile(filepath.Join(dir, "Artist - First.mp3"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: tracks}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("Download Playlist"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(a.downloads.history) == 3 && len(a.downloads.batches) == 0 })
	if a.toast != "" {
		t.Fatalf("download progress used a toast: %q", a.toast)
	}
	for name, want := range map[string]string{"Artist - First.mp3": "old", "Artist - First (1).mp3": "audio", "Artist - Last.mp3": "audio"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
	}
}

// A configured folder sends a song menu download to the queue without a save dialog.
func TestSongDownloadUsesQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	dir := t.TempDir()
	a.settings.DownloadFolder = dir
	fake(a).stream = func(id, quality string) (*music.Stream, error) { return &music.Stream{URL: srv.URL, Type: "mp3"}, nil }
	real := saveSongAs
	saveSongAs = func(*app, string) string { t.Error("opened save dialog"); return "" }
	defer func() { saveSongAs = real }()
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: []music.Track{song("1", "First", "Artist")}}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.RightClick("First"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Download"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(a.downloads.history) == 1 && len(a.downloads.batches) == 0 })
	b, err := os.ReadFile(filepath.Join(dir, "Artist - First.mp3"))
	if err != nil || string(b) != "audio" {
		t.Fatalf("saved %q: %v", b, err)
	}
}

// A playlist uses each song's own source and counts unknown and unavailable songs as failures.
func TestDownloadPlaylistSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(r.URL.Path)) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		return &music.Stream{URL: srv.URL + "/first", Type: "mp3"}, nil
	}
	other := newFakeSource("other")
	other.stream = func(id, quality string) (*music.Stream, error) {
		return &music.Stream{URL: srv.URL + "/other", Type: "mp3"}, nil
	}
	addFake(a, other)
	first := song("1", "First")
	first.Source = "fake"
	second := song("2", "Second")
	second.Source = "other"
	missing := song("3", "Missing")
	missing.Source = "missing"
	unavailable := song("4", "Unavailable")
	unavailable.Unavailable = true
	a.downloadPlaylist(music.Playlist{Tracks: []music.Track{first, second, missing, unavailable}})
	until(t, ch, func() bool { return len(a.downloads.history) == 4 && len(a.downloads.batches) == 0 })
	for name, want := range map[string]string{"First.mp3": "/first", "Second.mp3": "/other"} {
		b, err := os.ReadFile(filepath.Join(a.settings.DownloadFolder, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
	}
}

// A single song waits behind a busy playlist and a full queue rejects another batch.
func TestDownloadQueueBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		if id == "block" {
			started <- struct{}{}
			<-release
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	tracks := []music.Track{song("block", "One"), song("block", "Two"), song("block", "Three")}
	a.downloadPlaylist(music.Playlist{Tracks: tracks})
	for range tracks {
		wait(t, started, "download to start")
	}
	a.download(song("single", "Single"))
	if len(a.downloads.batches) != 2 || a.downloads.active != 3 {
		t.Fatalf("queue: %d batches, %d active", len(a.downloads.batches), a.downloads.active)
	}
	for i := 0; i < 6; i++ {
		a.download(song("single", "Single"))
	}
	a.download(song("extra", "Extra"))
	if len(a.downloads.batches) != 8 || a.toast != "Download queue is full" {
		t.Fatalf("queue: %d batches, toast %q", len(a.downloads.batches), a.toast)
	}
	close(release)
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	if _, err := os.Stat(filepath.Join(a.settings.DownloadFolder, "Extra.mp3")); !os.IsNotExist(err) {
		t.Fatalf("rejected download exists: %v", err)
	}
}

// Choosing a download folder keeps it in the settings file for the next launch.
func TestDownloadFolderSetting(t *testing.T) {
	a := newTestApp()
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	dir := t.TempDir()
	real := pickFolders
	pickFolders = func(_ *app, got func([]string)) { got([]string{dir}) }
	defer func() { pickFolders = real }()
	a.settingsPage = "playback"
	tt := ui.NewTester(a.settingsView, 780, 900)
	tt.Frame()
	if err := tt.Click("Choose Folder…"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.settings.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved settings
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DownloadFolder != dir {
		t.Fatalf("saved folder %q, want %q", saved.DownloadFolder, dir)
	}
}

// A transient failure is retried automatically and succeeds without another click.
func TestDownloadRetriesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	var calls atomic.Int32
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		if calls.Add(1) < 3 {
			return nil, errors.New("temporary error")
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	a.download(song("1", "Retry automatically"))
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	job := a.downloads.history[0]
	if job.state != "done" || calls.Load() != 3 || job.attempts != 3 {
		t.Fatalf("state %s, attempts %d, calls %d", job.state, job.attempts, calls.Load())
	}
}

// A timed-out download waits for the listener to retry it from the download list.
func TestDownloadTimeoutManualRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	var calls atomic.Int32
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		if calls.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	a.download(song("1", "Timed song"))
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	job := a.downloads.history[0]
	if job.state != "timeout" || calls.Load() != 1 {
		t.Fatalf("state %s, calls %d", job.state, calls.Load())
	}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("Downloads"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Retry Timed song"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return job.state == "done" })
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
	tt.Frame()
	if err := tt.Click("Clear Completed"); err != nil {
		t.Fatal(err)
	}
	if len(a.downloads.history) != 0 {
		t.Fatal("completed download still listed")
	}
	if b, err := os.ReadFile(filepath.Join(a.settings.DownloadFolder, "Timed song.mp3")); err != nil || string(b) != "audio" {
		t.Fatalf("saved %q: %v", b, err)
	}
}

// Changing download concurrency is saved and starts another waiting download.
func TestDownloadConcurrencySetting(t *testing.T) {
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	a.settings.DownloadConcurrency = 1
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	release := make(chan struct{})
	started := make(chan string, 2)
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		started <- id
		<-release
		return nil, music.ErrUnavailable
	}
	a.downloadPlaylist(music.Playlist{Tracks: []music.Track{song("1", "One"), song("2", "Two")}})
	wait(t, started, "first download")
	a.settingsPage = "playback"
	tt := ui.NewTester(a.settingsView, 780, 1000)
	tt.Frame()
	clickSettingRole(t, tt, ui.RolePopUpButton, "Concurrent Downloads")
	if err := tt.Click("2"); err != nil {
		t.Fatal(err)
	}
	wait(t, started, "second download")
	b, err := os.ReadFile(a.settings.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved settings
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DownloadConcurrency != 2 {
		t.Fatalf("saved concurrency %d", saved.DownloadConcurrency)
	}
	close(release)
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
}

// chooseDownloadSong moves over a row and clicks its checkbox.
func chooseDownloadSong(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	r, ok := tt.Find(name)
	if !ok {
		t.Fatalf("no song %q", name)
	}
	tt.Move(r.X+r.W/2, r.Y+r.H/2)
	tt.Frame()
	if err := tt.Click("Select " + name); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
}

// Selecting songs queues only the chosen rows and counts an unavailable choice as failed.
func TestDownloadSelectedSongs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		return &music.Stream{URL: srv.URL + "/" + id, Type: "mp3"}, nil
	}
	unavailable := song("3", "Unavailable")
	unavailable.Unavailable = true
	tracks := []music.Track{song("1", "First"), song("2", "Second"), unavailable}
	a.playlists["42"] = &music.Playlist{ID: "42", TrackCount: 4, Tracks: tracks}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	before, ok := tt.Find("First")
	if !ok {
		t.Fatal("no first song")
	}
	if err := tt.RightClick("First"); err != nil {
		t.Fatal(err)
	}
	if a.selection.active {
		t.Fatal("right click selected a song")
	}
	tt.CloseMenu()
	tt.Frame()
	chooseDownloadSong(t, tt, "First")
	after, ok := tt.Find("First")
	if !ok || before.Y != after.Y || before.H != after.H {
		t.Fatal("selection moved or resized the row")
	}
	if _, ok := tt.Find("Download Playlist"); ok {
		t.Fatal("playlist download stayed visible after selecting a song")
	}
	if _, ok := tt.Find("Download Selected (1)"); !ok {
		t.Fatal("selection download did not replace playlist download")
	}
	chooseDownloadSong(t, tt, "Unavailable")
	if a.pl.Current() != nil {
		t.Fatal("selecting a row started playback")
	}
	if err := tt.Click("Download Selected (2)"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	if len(a.downloads.history) != 2 || a.downloads.history[0].state != "done" || a.downloads.history[1].state != "failed" {
		t.Fatalf("downloads: %+v", a.downloads.history)
	}
	if b, err := os.ReadFile(filepath.Join(a.settings.DownloadFolder, "First.mp3")); err != nil || string(b) != "audio" {
		t.Fatalf("first: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(a.settings.DownloadFolder, "Second.mp3")); !os.IsNotExist(err) {
		t.Fatalf("unselected file: %v", err)
	}
}

// Select all and cancel leave the normal click-to-play action intact.
func TestDownloadSelectionControls(t *testing.T) {
	a := newTestApp()
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: []music.Track{song("1", "First"), song("2", "Second")}}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if a.selection.active {
		t.Fatal("selection started without input")
	}
	chooseDownloadSong(t, tt, "First")
	if err := tt.Click("Select All"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(a.selection.chosen) != 2 {
		t.Fatalf("selected %d", len(a.selection.chosen))
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.selection.active {
		t.Fatal("selection mode remained active")
	}
	if err := tt.Click("First"); err != nil {
		t.Fatal(err)
	}
	if a.pl.Current() == nil || a.pl.Current().ID != "1" {
		t.Fatal("normal click did not play the song")
	}
}

// Navigating away or replacing a playlist clears the selection before another list can use it.
func TestDownloadSelectionClearsOnListChange(t *testing.T) {
	a := newTestApp()
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: []music.Track{song("1", "First"), song("2", "Second")}}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	chooseDownloadSong(t, tt, "First")
	a.playlists["42"].Tracks = []music.Track{song("2", "Second"), song("1", "First")}
	tt.Frame()
	if a.selection.active {
		t.Fatal("selection survived the playlist changing")
	}
	chooseDownloadSong(t, tt, "Second")
	a.open(route{page: pageHome, src: "fake"})
	if a.selection.active {
		t.Fatal("selection survived navigation")
	}
}

// A selected song from a remote source remains downloadable on a local playlist.
func TestDownloadSelectionMixedSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	fake(a).stream = func(id, quality string) (*music.Stream, error) { return &music.Stream{URL: srv.URL, Type: "mp3"}, nil }
	localSource := newFakeSource(local.ID)
	addFake(a, localSource)
	a.library = a.libOf(local.ID)
	tr := song("1", "Remote Song")
	tr.Source = "fake"
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: []music.Track{tr}}
	a.route = route{page: pagePlaylist, src: local.ID, id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	chooseDownloadSong(t, tt, "Remote Song")
	if err := tt.Click("Download Selected (1)"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	if len(a.downloads.history) != 1 || a.downloads.history[0].state != "done" {
		t.Fatal("remote song did not download")
	}
}

// Selecting a liked song also lets the listener download the chosen rows.
func TestDownloadSelectionLikedSongs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	a.settings.DownloadFolder = t.TempDir()
	fake(a).stream = func(id, quality string) (*music.Stream, error) { return &music.Stream{URL: srv.URL, Type: "mp3"}, nil }
	a.likedPaged.loaded = true
	a.likedList = []music.Track{song("1", "Liked First"), song("2", "Liked Second")}
	a.route = route{page: pageLiked, src: "fake"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	chooseDownloadSong(t, tt, "Liked First")
	if err := tt.Click("Download Selected (1)"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(a.downloads.batches) == 0 })
	if len(a.downloads.history) != 1 || a.downloads.history[0].track.ID != "1" || a.downloads.history[0].state != "done" {
		t.Fatal("the selected liked song was not downloaded")
	}
}

// The download page keeps failed tasks when completed tasks are cleared and updates its counts.
func TestDownloadPageClearCompleted(t *testing.T) {
	a := newTestApp()
	done := &downloadJob{track: song("1", "Done"), state: "done"}
	failed := &downloadJob{track: song("2", "Failed"), state: "failed", err: music.ErrUnavailable}
	waiting := &downloadJob{track: song("3", "Waiting"), state: "queued"}
	a.downloads.history = []*downloadJob{done, failed, waiting}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("Downloads"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.route.page != pageDownloads {
		t.Fatal("download page did not open")
	}
	if doneCount, total := a.downloadProgress(); doneCount != 2 || total != 3 {
		t.Fatalf("progress %d/%d", doneCount, total)
	}
	if err := tt.Click("Clear Completed"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tt.Frame()
	if len(a.downloads.history) != 2 || a.downloads.history[0] != failed || a.downloads.history[1] != waiting {
		t.Fatal("clearing completed tasks changed other tasks")
	}
	if doneCount, total := a.downloadProgress(); doneCount != 1 || total != 2 {
		t.Fatalf("progress %d/%d", doneCount, total)
	}
	tt.Key(ui.Cmd, ui.KeyBracketLeft)
	tt.Frame()
	if a.route.page != pageHome {
		t.Fatal("back did not leave the download page")
	}
}

// A timeout while choosing a single song's save path stays in the list for manual retry.
func TestDownloadBeforeSaveDialogTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) }))
	defer srv.Close()
	a := newTestApp()
	ch := posts(a)
	dir := t.TempDir()
	real := saveSongAs
	saveSongAs = func(*app, string) string { return filepath.Join(dir, "picked.mp3") }
	defer func() { saveSongAs = real }()
	var calls atomic.Int32
	fake(a).stream = func(id, quality string) (*music.Stream, error) {
		if calls.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return &music.Stream{URL: srv.URL, Type: "mp3"}, nil
	}
	a.download(song("1", "Timed"))
	until(t, ch, func() bool { return len(a.downloads.history) == 1 && a.downloads.history[0].state == "timeout" })
	if a.toast != "" || calls.Load() != 1 {
		t.Fatalf("toast %q, calls %d", a.toast, calls.Load())
	}
	job := a.downloads.history[0]
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("Downloads"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Retry Timed"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return job.state == "done" })
	if len(a.downloads.history) != 1 {
		t.Fatal("retry duplicated the task")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "picked.mp3")); err != nil || string(b) != "audio" {
		t.Fatalf("saved %q: %v", b, err)
	}
}

// The checkbox toggles selection without playing, while clicking the song still plays it.
func TestDownloadCheckboxAndPlayback(t *testing.T) {
	a := newTestApp()
	a.playlists["42"] = &music.Playlist{ID: "42", Tracks: []music.Track{song("1", "First"), song("2", "Second")}}
	a.route = route{page: pagePlaylist, src: "fake", id: "42"}
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	chooseDownloadSong(t, tt, "First")
	if a.pl.Current() != nil || !a.selection.chosen[0] {
		t.Fatal("checkbox did not select without playing")
	}
	chooseDownloadSong(t, tt, "First")
	if a.selection.active {
		t.Fatal("unchecking the last song kept the selection")
	}
	chooseDownloadSong(t, tt, "Second")
	if err := tt.Click("First"); err != nil {
		t.Fatal(err)
	}
	if a.pl.Current() == nil || a.pl.Current().ID != "1" {
		t.Fatal("clicking the song while selecting did not play it")
	}
	if !a.selection.chosen[1] {
		t.Fatal("playing changed the selection")
	}
}
