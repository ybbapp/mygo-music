package app

import (
	"errors"
	"fmt"
	"slices"

	"github.com/egoist/mygo/ui"
	"mygo-music/internal/music"
)

// downloadProgress counts finished attempts, including failures, in this session.
func (a *app) downloadProgress() (finished, total int) {
	for _, job := range a.downloads.history {
		total++
		if job.state == "done" || job.state == "failed" || job.state == "timeout" {
			finished++
		}
	}
	return
}

// downloadNav opens the download page and shows how much of its list finished.
func (a *app) downloadNav(c *ui.Context) {
	p := a.pal
	selected := a.route.page == pageDownloads && !a.nowPlaying
	b := ui.ButtonBase(c.Key("downloads")).Height(34).PaddingX(10).Gap(10).Radius(8).AlignItems(ui.Center).Justify(ui.Start).FocusRing(false).Label(T("Downloads"))
	bg := ui.Transparent
	if selected {
		bg = p.press
	} else if b.Hovered() {
		bg = p.hover
	}
	b.Background(bg)
	b.Children(func() {
		color := p.text
		if selected {
			color = p.accent
		}
		ui.Icon(c, iconDownload).Size(17, 17).TextColor(color)
		ui.Text(c, T("Downloads")).FontSize(13.5).FontWeight(500).TextColor(p.text)
		ui.Box(c).Grow(1)
		done, total := a.downloadProgress()
		if total > 0 {
			ui.Text(c, fmt.Sprintf("%d/%d", done, total)).FontSize(11.5).FontWeight(600).FontFeatures("tnum").TextColor(p.muted)
		}
	})
	if b.Clicked() {
		a.open(route{page: pageDownloads})
	}
}

// downloadPage lists the session's songs like the other song pages.
func (a *app) downloadPage(c *ui.Context) {
	p := a.pal
	jobs := a.downloads.history
	top, bottom := a.insets(c)
	a.underTop, a.underBottom = true, true
	ui.List(c.Key("download-list"), &a.trackList, len(jobs)+1).
		Grow(1).Padding(top+pageGap, pagePad-12, bottom+32, pagePad-12).ScrollbarInsets(top, 0, bottom, 0).
		ItemKey(func(i int) any {
			if i == 0 {
				return "downloads-header"
			}
			return jobs[i-1]
		}).
		Rows(func(row ui.ListRow) {
			c := row.Context
			if row.Index == 0 {
				ui.Column(c).Children(func() {
					a.startMark(c)
					ui.Row(c).Padding(0, 12, 22, 12).Gap(12).AlignItems(ui.End).Children(func() {
						ui.Column(c).Grow(1).Gap(4).Children(func() {
							ui.Text(c, T("Downloads")).FontSize(30).FontWeight(700).LetterSpacing(-0.6)
							if len(jobs) > 0 {
								done, total := a.downloadProgress()
								ui.Text(c, Tf("%d of %d finished", done, total)).FontSize(13).TextColor(p.muted)
							}
						})
						if len(jobs) > 0 && pillButton(c.Key("clear-completed"), nil, T("Clear Completed"), p.hover.Over(p.bg), p.text).Clicked() {
							a.downloads.history = slices.DeleteFunc(slices.Clone(a.downloads.history), func(job *downloadJob) bool { return job.state == "done" })
							a.saveDownloads()
						}
					})
					if len(jobs) == 0 {
						ui.Text(c, T("No downloads yet")).FontSize(13.5).TextColor(p.muted).Margin(20, 12, 0, 12)
					}
				})
				if len(jobs) == 0 {
					a.endMark(c)
				}
				return
			}
			job := jobs[row.Index-1]
			if row.Index == len(jobs) {
				defer a.endMark(c)
			}
			ui.Row(c.Key(job)).Height(60).PaddingX(12).Gap(14).Radius(10).AlignItems(ui.Center).Children(func() {
				ui.Textf(c, "%d", row.Index).FontSize(13).FontFeatures("tnum").TextColor(p.faint).Width(26).TextAlign(ui.Center)
				a.cover(c, job.track.Album.ImageURL, 96, 38, 6)
				ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(3).Children(func() {
					ui.Text(c, job.track.Name).FontSize(14).FontWeight(500).SingleLine()
					ui.Text(c, job.track.ArtistNames()).FontSize(12.5).TextColor(p.muted).SingleLine()
				})
				status := "Waiting"
				switch job.state {
				case "downloading":
					status = "Downloading…"
				case "done":
					status = "Downloaded"
				case "failed":
					switch {
					case errors.Is(job.err, music.ErrUnavailable), errors.Is(job.err, music.ErrUnsupported):
						status = "Unavailable"
					case errors.Is(job.err, music.ErrSignIn):
						status = "Sign in required"
					case errors.Is(job.err, music.ErrSetup):
						status = "Setup required"
					case errors.Is(job.err, errDownloadNeedsLocation):
						status = "Choose a save location"
					case errors.Is(job.err, errDownloadQueueFull):
						status = "Download queue is full"
					default:
						status = "Failed"
					}
				case "timeout":
					status = "Timed out"
				}
				ui.Text(c, T(status)).FontSize(12.5).TextColor(p.muted).SingleLine()
				if job.state == "failed" || job.state == "timeout" {
					if errors.Is(job.err, music.ErrSignIn) && job.lib != nil {
						if ui.Button(c.Key("sign-in"), T("Sign In")).Clicked() {
							a.promptSignIn(job.lib)
						}
					} else if errors.Is(job.err, music.ErrSetup) && job.lib != nil {
						if ui.Button(c.Key("setup"), T("Settings")).Clicked() {
							a.openSourceSettings()
						}
					}
					if ui.Button(c.Key("retry"), T("Retry")).Label(Tf("Retry %s", job.track.Name)).Clicked() {
						a.retryDownload(job)
					}
				} else {
					ui.Box(c).Width(56)
				}
			})
		})
}
