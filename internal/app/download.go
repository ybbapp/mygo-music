package app

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo"

	"mygo-music/internal/local"
	"mygo-music/internal/music"
	"mygo-music/internal/player"
)

// saveSongAs asks the listener where to save a song with the native dialog,
// suggesting name in their Downloads folder, and returns the path, or "" if
// they didn't pick one. It blocks, so it runs on a goroutine; tests replace it.
var saveSongAs = func(a *app, name string) string {
	dir, _ := mygo.App.Path(mygo.PathDownloads)
	path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
		Parent:            a.win,
		Title:             T("Download"),
		DefaultPath:       filepath.Join(dir, name),
		ButtonLabel:       T("Download"),
		CreateDirectories: true,
	})
	if err != nil {
		return ""
	}
	return path
}

// canDownload reports whether tr can be saved to a file of the listener's: a
// song its source can hand over as audio, rather than one already on disk.
// A source that plays elsewhere may still stream for this, as Spotify does
// through librespot, so where it plays isn't what decides.
func (a *app) canDownload(tr *music.Track) bool {
	lib := a.downloadLib(tr)
	return lib != nil && lib.id != local.ID && lib.can("stream") && !tr.Unavailable
}

// downloadLib resolves an explicitly named source without falling back to another.
func (a *app) downloadLib(tr *music.Track) *library {
	if tr.Source != "" {
		return a.libOf(tr.Source)
	}
	return a.library
}

// download saves a copy of tr where the listener picks, at the quality the
// settings say, from the cache if it's kept there.
func (a *app) download(tr music.Track) {
	if a.downloads.store != nil && !a.downloads.loaded {
		if tr.Source == "" && a.library != nil {
			tr.Source = a.library.id
		}
		a.downloads.deferred = append(a.downloads.deferred, func() { a.download(tr) })
		return
	}
	if a.settings.DownloadFolder != "" {
		a.enqueueDownloads(a.downloadJobs([]music.Track{tr}), a.settings.DownloadFolder)
		return
	}
	job := a.downloadJobs([]music.Track{tr})[0]
	if _, skipped := a.uniqueDownloads([]*downloadJob{job}); skipped > 0 {
		a.duplicateDownloads(skipped)
		return
	}
	job.quality = a.settings.Quality
	a.downloads.history = append(a.downloads.history, job)
	a.prepareSingle(job)
}

// prepareSingle finds a suggested extension before the save dialog; a failure stays in the list.
func (a *app) prepareSingle(job *downloadJob) {
	job.state, job.err, job.attempts = "downloading", nil, 0
	a.saveDownloads()
	go func() {
		var song *player.Song
		var err error
		attempts := 0
		for attempts < 3 {
			attempts++
			if job.lib == nil {
				err = music.ErrUnavailable
			} else {
				song, err = a.pl.FindSong(job.lib.src, job.track, job.quality)
			}
			if err == nil || !downloadRetryable(err) {
				break
			}
		}
		if err != nil {
			a.post(func() {
				job.err, job.attempts = err, attempts
				if downloadTimedOut(err) {
					job.state = "timeout"
				} else {
					job.state = "failed"
				}
				a.saveDownloads()
			})
			return
		}
		path := saveSongAs(a, songFileName(&job.track, song.Type))
		song.Close()
		a.post(func() {
			if path == "" {
				a.downloads.history = slices.DeleteFunc(slices.Clone(a.downloads.history), func(other *downloadJob) bool { return other == job })
				a.saveDownloads()
				return
			}
			job.path, job.dir, job.state = path, filepath.Dir(path), "queued"
			a.queueExisting(job)
		})
	}()
}

var errDownloadQueueFull = errors.New("download queue is full")

// queueExisting keeps a previously listed song in the bounded queue.
func (a *app) queueExisting(job *downloadJob) {
	if len(a.downloads.batches) >= 8 {
		job.state, job.err = "failed", errDownloadQueueFull
		a.saveDownloads()
		return
	}
	a.downloads.batches = append(a.downloads.batches, &downloadBatch{jobs: []*downloadJob{job}})
	a.saveDownloads()
	a.runDownloads()
}

// songFileName names the file of a song of type ext as its credits read,
// "Artists - Title.ext", with what no file name may hold taken out.
func songFileName(tr *music.Track, ext string) string {
	name := tr.Name
	if artists := tr.ArtistNames(); artists != "" {
		name = artists + " - " + name
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	// Most systems take 255 bytes, with room left for the extension.
	for len(name) > 200 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	// Windows drops a trailing dot or space, and a leading dot hides the file.
	name = strings.Trim(name, ". ")
	if name == "" {
		name = T("Song")
	}
	return name + "." + ext
}

// downloadQueue bounds concurrent saves and keeps this session’s tasks.
type downloadQueue struct {
	batches  []*downloadBatch
	active   int
	history  []*downloadJob
	deferred []func()
	store    *downloadStore
	loaded   bool
}
type downloadBatch struct {
	jobs       []*downloadJob
	next, done int
}

type downloadJob struct {
	track        music.Track
	lib          *library
	dir, quality string
	path         string // an explicit path chosen for a single song
	safePath     bool   // a restored path must not replace a file created while away
	state        string
	attempts     int
	err          error
}

func (a *app) downloadJobs(tracks []music.Track) []*downloadJob {
	jobs := make([]*downloadJob, len(tracks))
	for i := range tracks {
		tr := tracks[i]
		if tr.Source == "" && a.library != nil {
			tr.Source = a.library.id
		}
		jobs[i] = &downloadJob{track: tr, state: "queued"}
		if a.canDownload(&tr) {
			jobs[i].lib = a.downloadLib(&tr)
		}
	}
	return jobs
}

// downloadIdentity is source-scoped: different providers may use the same ID.
type downloadIdentity struct{ source, id string }

// uniqueDownloads skips tasks already waiting, running or completed, and repeats in the request.
func (a *app) uniqueDownloads(jobs []*downloadJob) ([]*downloadJob, int) {
	seen := make(map[downloadIdentity]bool, len(a.downloads.history)+len(jobs))
	for _, old := range a.downloads.history {
		if old.track.ID != "" && old.track.Source != "" && (old.state == "queued" || old.state == "downloading" || old.state == "done") {
			seen[downloadIdentity{old.track.Source, old.track.ID}] = true
		}
	}
	unique := make([]*downloadJob, 0, len(jobs))
	positions := make(map[downloadIdentity]int, len(jobs))
	skipped := 0
	for _, job := range jobs {
		key := downloadIdentity{job.track.Source, job.track.ID}
		if key.source != "" && key.id != "" {
			if seen[key] {
				skipped++
				continue
			}
			if at, ok := positions[key]; ok {
				// A source may mark one occurrence unavailable but supply a playable one later.
				if unique[at].lib == nil && job.lib != nil {
					unique[at] = job
				}
				skipped++
				continue
			}
			positions[key] = len(unique)
		}
		unique = append(unique, job)
	}
	return unique, skipped
}

func (a *app) duplicateDownloads(n int) {
	if n > 0 {
		a.toast = Tf("Skipped %d duplicate downloads", n)
	}
}

func (a *app) enqueueDownloads(jobs []*downloadJob, dir string) {
	jobs, skipped := a.uniqueDownloads(jobs)
	a.duplicateDownloads(skipped)
	if len(jobs) == 0 {
		return
	}
	if len(a.downloads.batches) >= 8 {
		a.toast = T("Download queue is full")
		return
	}
	for _, job := range jobs {
		job.dir, job.quality = dir, a.settings.Quality
	}
	a.downloads.history = append(a.downloads.history, jobs...)
	a.downloads.batches = append(a.downloads.batches, &downloadBatch{jobs: jobs})
	a.saveDownloads()
	a.runDownloads()
}

func (a *app) downloadPlaylist(pl music.Playlist) {
	if pl.TrackCount > len(pl.Tracks) {
		a.toast = T("This playlist is incomplete and cannot be downloaded in full")
		return
	}
	a.downloadTracks(pl.Tracks)
}

// downloadTracks queues exactly the tracks chosen, asking for a folder if needed.
func (a *app) downloadTracks(tracks []music.Track) {
	jobs := a.downloadJobs(tracks)
	if a.downloads.store != nil && !a.downloads.loaded {
		a.downloads.deferred = append(a.downloads.deferred, func() { a.submitDownloadJobs(jobs) })
		return
	}
	a.submitDownloadJobs(jobs)
}

func (a *app) submitDownloadJobs(jobs []*downloadJob) {
	jobs, skipped := a.uniqueDownloads(jobs)
	a.duplicateDownloads(skipped)
	if len(jobs) == 0 {
		return
	}
	start := func(dir string) { a.enqueueDownloads(jobs, dir) }
	if a.settings.DownloadFolder != "" {
		start(a.settings.DownloadFolder)
		return
	}
	pickFolders(a, func(paths []string) {
		if len(paths) > 0 {
			a.settings.DownloadFolder = paths[0]
			a.changed()
			start(paths[0])
		}
	})
}

func (a *app) runDownloads() {
	q := &a.downloads
	limit := a.settings.DownloadConcurrency
	if limit < 1 || limit > 8 {
		limit = 3
	}
	for q.active < limit && len(q.batches) > 0 {
		var b *downloadBatch
		for _, pending := range q.batches {
			if pending.next < len(pending.jobs) {
				b = pending
				break
			}
		}
		if b == nil {
			break
		}
		job := b.jobs[b.next]
		tr := job.track
		job.state = "downloading"
		b.next++
		q.active++
		go func() {
			var err error
			attempts := 0
			for attempts < 3 {
				attempts++
				if job.lib == nil {
					err = music.ErrUnavailable
				} else {
					var song *player.Song
					song, err = a.pl.FindSong(job.lib.src, tr, job.quality)
					if err == nil {
						if job.path != "" && job.safePath {
							err = song.SaveUnique(filepath.Dir(job.path), filepath.Base(job.path))
						} else if job.path != "" {
							err = song.SaveTo(job.path)
						} else {
							err = song.SaveUnique(job.dir, songFileName(&tr, song.Type))
						}
					}
				}
				if err == nil || !downloadRetryable(err) {
					break
				}
			}

			a.post(func() {
				q.active--
				job.attempts, job.err = attempts, err
				switch {
				case err == nil:
					job.state = "done"
				case downloadTimedOut(err):
					job.state = "timeout"
				default:
					job.state = "failed"
				}
				b.done++
				if b.done == len(b.jobs) {
					for i, batch := range q.batches {
						if batch == b {
							q.batches = append(q.batches[:i], q.batches[i+1:]...)
							break
						}
					}
				}
				a.saveDownloads()
				a.runDownloads()
			})
		}()
	}
}

// downloadRetryable leaves timeouts and errors needing listener action for manual retry.
func downloadRetryable(err error) bool {
	return !downloadTimedOut(err) && !errors.Is(err, music.ErrUnavailable) && !errors.Is(err, music.ErrUnsupported) && !errors.Is(err, music.ErrSignIn) && !errors.Is(err, music.ErrSetup)
}

// downloadTimedOut distinguishes timeouts, which wait for the listener to retry.
func downloadTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (a *app) retryDownload(job *downloadJob) {
	if job.state != "failed" && job.state != "timeout" {
		return
	}
	if _, skipped := a.uniqueDownloads([]*downloadJob{job}); skipped > 0 {
		a.duplicateDownloads(skipped)
		return
	}
	job.lib = nil
	if a.canDownload(&job.track) {
		job.lib = a.downloadLib(&job.track)
	}
	if job.path == "" && job.dir == "" {
		a.prepareSingle(job)
		return
	}
	job.state, job.attempts, job.err = "queued", 0, nil
	a.queueExisting(job)
}
