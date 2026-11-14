package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"

	"mygo-music/internal/music"
)

// savedDownload keeps only the information needed to show or resume a task.
type savedDownload struct {
	Track    music.Track `json:"track"`
	Dir      string      `json:"dir,omitempty"`
	Path     string      `json:"path,omitempty"`
	Quality  string      `json:"quality,omitempty"`
	State    string      `json:"state"`
	Attempts int         `json:"attempts,omitempty"`
	Problem  string      `json:"problem,omitempty"`
}

type downloadWrite struct {
	jobs []savedDownload
	done chan struct{}
}
type downloadStore struct {
	path   string
	writes chan downloadWrite
	report func(error)
}

// startDownloadStore reads before enabling writes, so a new task cannot erase the old list.
func (a *app) startDownloadStore(path string) {
	store := &downloadStore{path: path, writes: make(chan downloadWrite, 1)}
	store.report = func(err error) { a.post(func() { a.toast = T("Couldn't save the download list") }) }
	a.downloads.store = store
	go func() {
		b, err := os.ReadFile(path)
		var saved []savedDownload
		if err == nil {
			if err = json.Unmarshal(b, &saved); err != nil {
				log.Printf("loading downloads: %v", err)
				a.post(func() {
					if a.downloads.store == store {
						a.downloads.store = nil
						a.runDeferredDownloads()
						a.toast = T("Couldn't load the download list")
					}
				})
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("loading downloads: %v", err)
			a.post(func() {
				if a.downloads.store == store {
					a.downloads.store = nil
					a.runDeferredDownloads()
					a.toast = T("Couldn't load the download list")
				}
			})
			return
		}
		go store.run()
		a.post(func() {
			a.downloads.loaded = true
			a.restoreDownloads(saved)
			a.runDeferredDownloads()
			a.saveDownloads()
			a.runDownloads()
		})
	}()
}

// runDeferredDownloads admits requests made while the saved list was loading.
func (a *app) runDeferredDownloads() {
	deferred := a.downloads.deferred
	a.downloads.deferred = nil
	for _, start := range deferred {
		start()
	}
}

// run serializes writes away from the UI thread and keeps the last complete file.
func (s *downloadStore) run() {
	for w := range s.writes {
		if err := writeDownloads(s.path, w.jobs); err != nil {
			log.Printf("saving downloads: %v", err)
			if s.report != nil {
				go s.report(err)
			}
		}
		if w.done != nil {
			close(w.done)
		}
	}
}

func writeDownloads(path string, jobs []savedDownload) error {
	b, err := json.Marshal(jobs)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".downloads-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// savedDownloads takes a private snapshot; its slices may be encoded by the writer.
func (a *app) savedDownloads() []savedDownload {
	out := make([]savedDownload, 0, len(a.downloads.history))
	for _, job := range a.downloads.history {
		tr := job.track
		tr.Artists = slices.Clone(tr.Artists)
		tr.Album.Artists = slices.Clone(tr.Album.Artists)
		problem := ""
		switch {
		case errors.Is(job.err, music.ErrSignIn):
			problem = "signIn"
		case errors.Is(job.err, music.ErrSetup):
			problem = "setup"
		case errors.Is(job.err, music.ErrUnsupported):
			problem = "unsupported"
		case errors.Is(job.err, music.ErrUnavailable):
			problem = "unavailable"
		case errors.Is(job.err, errDownloadQueueFull):
			problem = "queueFull"
		case errors.Is(job.err, errDownloadNeedsLocation):
			problem = "location"
		}
		out = append(out, savedDownload{Track: tr, Dir: job.dir, Path: job.path, Quality: job.quality, State: job.state, Attempts: job.attempts, Problem: problem})
	}
	return out
}

// saveDownloads coalesces snapshots when the disk is slower than the queue.
func (a *app) saveDownloads() {
	s := a.downloads.store
	if s == nil || !a.downloads.loaded {
		return
	}
	w := downloadWrite{jobs: a.savedDownloads()}
	select {
	case s.writes <- w:
		return
	default:
	}
	select {
	case <-s.writes:
	default:
	}
	s.writes <- w
}

// flushDownloads waits for the final snapshot on quit; writes themselves run off the UI thread.
func (a *app) flushDownloads() {
	s := a.downloads.store
	if s == nil || !a.downloads.loaded {
		return
	}
	done := make(chan struct{})
	s.writes <- downloadWrite{jobs: a.savedDownloads(), done: done}
	<-done
	close(s.writes)
	a.downloads.store = nil
}

var errDownloadNeedsLocation = errors.New("download needs a save location")

// restoreDownloads puts interrupted jobs back in one bounded batch; finished failures wait for retry.
func (a *app) restoreDownloads(saved []savedDownload) {
	restored := make([]*downloadJob, 0, len(saved))
	pending := make([]*downloadJob, 0)
	// Older lists could contain the same song more than once. A completed
	// copy takes precedence, then the first interrupted task is resumed.
	seen := make(map[downloadIdentity]bool, len(saved))
	for _, item := range saved {
		if item.State == "done" && item.Track.Source != "" && item.Track.ID != "" {
			seen[downloadIdentity{item.Track.Source, item.Track.ID}] = true
		}
	}
	skipped := 0
	for _, item := range saved {
		if (item.State == "queued" || item.State == "downloading") && item.Track.Source != "" && item.Track.ID != "" {
			key := downloadIdentity{item.Track.Source, item.Track.ID}
			if seen[key] {
				skipped++
				continue
			}
			// Keep a later usable copy when this one cannot run.
			if (item.Dir != "" || item.Path != "") && a.canDownload(&item.Track) {
				seen[key] = true
			}
		}
		job := &downloadJob{track: item.Track, dir: item.Dir, path: item.Path, quality: item.Quality, state: item.State, attempts: item.Attempts}
		if a.canDownload(&job.track) {
			job.lib = a.downloadLib(&job.track)
		}
		switch item.Problem {
		case "signIn":
			job.err = music.ErrSignIn
		case "setup":
			job.err = music.ErrSetup
		case "unsupported":
			job.err = music.ErrUnsupported
		case "unavailable":
			job.err = music.ErrUnavailable
		case "queueFull":
			job.err = errDownloadQueueFull
		case "location":
			job.err = errDownloadNeedsLocation
		}
		if job.state == "timeout" {
			job.err = context.DeadlineExceeded
		}
		switch job.state {
		case "queued", "downloading":
			// A save-as path may have changed while the app was closed.
			if job.path != "" {
				job.safePath = true
			}
			if job.dir == "" && job.path == "" {
				job.state, job.err = "failed", errDownloadNeedsLocation
			} else if job.lib == nil {
				job.state, job.err = "failed", music.ErrUnavailable
			} else {
				job.state, job.err = "queued", nil
				pending = append(pending, job)
			}
		case "failed", "timeout":
			if job.path != "" {
				job.safePath = true
			}
		case "done":
		default:
			job.state, job.err = "failed", music.ErrUnavailable
		}
		restored = append(restored, job)
	}
	a.downloads.history = append(restored, a.downloads.history...)
	a.duplicateDownloads(skipped)
	if len(pending) > 0 {
		if len(a.downloads.batches) >= 8 {
			last := a.downloads.batches[len(a.downloads.batches)-1]
			last.jobs = append(last.jobs, pending...)
		} else {
			a.downloads.batches = append([]*downloadBatch{{jobs: pending}}, a.downloads.batches...)
		}
	}
}
