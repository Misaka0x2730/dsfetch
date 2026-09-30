package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"
	uatomic "go.uber.org/atomic"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
	"dsfetch/internal/transfer"
)

const (
	maxWalkDepth = 16
	maxWalkFiles = 20000
)

type queueStats struct {
	downloaded, unpacked, failed, skipped int
}

// Queue downloads a confirmed selection file by file (input: *QueueJob).
func (e *Env) Queue(in any) (any, error) {
	job, _ := in.(*QueueJob)
	s := e.session
	if job == nil || s == nil {
		return Nav{To: ScreenServers}, nil
	}
	back := Nav{To: ScreenBrowser, Input: job.Sel.Dir}

	var items []transfer.Item
	err := e.runTask(T("preparing"), true, func(ctx context.Context) error {
		var err error
		items, err = e.expand(ctx, job)
		return err
	})
	if e.QuitRequested() {
		return Exit, nil
	}
	if errors.Is(err, context.Canceled) {
		return back, nil
	}
	if err != nil {
		info(errorMessage(T("err_prepare"), err))
		return back, nil
	}
	if len(items) == 0 {
		info(T("nothing_to_download"))
		return back, nil
	}

	items, ok := e.resolveExisting(items)
	if !ok || len(items) == 0 {
		return back, nil
	}

	var total, largest int64
	for _, it := range items {
		if it.Size > 0 {
			total += it.Size - partSize(it)
			largest = max(largest, it.Size)
		}
	}
	if err := platform.CheckSpace(job.LocalDir, total+(1<<20), largest); err != nil {
		info(errorMessage(T("err_cannot_download"), err))
		return back, nil
	}

	// No sleep hold for the whole batch: downloadOne and unpacking hold the
	// console awake while they work, and questions in between (and the
	// summary) let a closed lid put it to sleep.
	dl := &transfer.Downloader{Connect: s.NewBackend, RateLimit: e.devThrottle()}
	defer dl.Close()

	var st queueStats
	for i, it := range items {
		err := e.downloadOne(dl, it, i, len(items))
		if e.QuitRequested() {
			return Exit, nil
		}
		if errors.Is(err, context.Canceled) {
			st.skipped += len(items) - i
			break
		}
		if err != nil {
			st.failed++
			slog.Error("download failed", "file", it.RemotePath, "err", err)
			msg := errorMessage(T("err_download", it.Name()), err)
			if i == len(items)-1 {
				info(msg)
				break
			}
			if !confirm(msg+"\n\n"+T("continue_rest_q"), T("continue"), T("stop")) {
				st.skipped += len(items) - i - 1
				break
			}
			continue
		}
		st.downloaded++
		slog.Info("downloaded", "file", it.RemotePath, "to", it.LocalPath, "size", it.Size)

		if job.Unpack {
			if arc, ready := extractableAfter(items, i); ready {
				ok, err := e.extractFlow(arc, filepath.Dir(arc))
				if e.QuitRequested() {
					return Exit, nil
				}
				if err != nil {
					st.failed++
				} else if ok {
					st.unpacked++
				}
			}
		}
	}

	if st.downloaded > 0 {
		e.Platform.RequestRescan()
	}
	summary := T("summary_downloaded", st.downloaded)
	if st.unpacked > 0 {
		summary += "\n" + T("summary_unpacked", st.unpacked)
	}
	if st.failed > 0 {
		summary += "\n" + T("summary_failed", st.failed)
	}
	if st.skipped > 0 {
		summary += "\n" + T("summary_skipped", st.skipped)
	}
	info(summary)
	return back, nil
}

// expand turns the selection into files, walking folders recursively and
// making local names safe for FAT32/exFAT.
func (e *Env) expand(ctx context.Context, job *QueueJob) ([]transfer.Item, error) {
	be := e.session.Backend
	var items []transfer.Item
	var walk func(remoteDir, localDir string, depth int) error
	walk = func(remoteDir, localDir string, depth int) error {
		if depth > maxWalkDepth {
			return fmt.Errorf("%s: %s", remoteDir, T("err_too_deep"))
		}
		entries, err := be.List(ctx, remoteDir)
		if err != nil {
			return err
		}
		remote.SortEntries(entries)
		for _, ent := range entries {
			if !e.Settings.ShowHidden && strings.HasPrefix(ent.Name, ".") {
				continue
			}
			rp := remote.Join(remoteDir, ent.Name)
			lp := filepath.Join(localDir, platform.SafeFileName(ent.Name))
			if ent.IsDir {
				if err := walk(rp, lp, depth+1); err != nil {
					return err
				}
				continue
			}
			size := ent.Size
			if ent.IsLink {
				size = -1
			}
			items = append(items, transfer.Item{RemotePath: rp, Size: size, LocalPath: lp})
			if len(items) > maxWalkFiles {
				return errors.New(T("err_too_many_files"))
			}
		}
		return nil
	}
	targetSys, targetKnown := e.Platform.SystemByDir(filepath.Base(job.LocalDir))
	for _, ent := range job.Sel.Entries {
		rp := remote.Join(job.Sel.Dir, ent.Name)
		lp := filepath.Join(job.LocalDir, platform.SafeFileName(ent.Name))
		if ent.IsDir {
			// Downloading the server's "GBA" folder into Roms/GBA must not
			// produce Roms/GBA/GBA: a folder of the same system is merged.
			if s, ok := e.Platform.SystemByDir(ent.Name); ok && targetKnown && s.Dir == targetSys.Dir {
				lp = job.LocalDir
			}
			if err := walk(rp, lp, 1); err != nil {
				return nil, err
			}
			continue
		}
		items = append(items, transfer.Item{RemotePath: rp, Size: ent.Size, LocalPath: lp})
	}
	return items, nil
}

// resolveExisting asks what to do with files that are already on the card.
func (e *Env) resolveExisting(items []transfer.Item) ([]transfer.Item, bool) {
	var exists int
	for _, it := range items {
		if fi, err := os.Stat(it.LocalPath); err == nil && !fi.IsDir() {
			exists++
		}
	}
	if exists == 0 {
		return items, true
	}
	idx, ok := choose(T("files_exist_q", exists, len(items)), []gaba.SelectionOption{
		{DisplayName: T("skip_existing"), Value: "skip"},
		{DisplayName: T("overwrite"), Value: "overwrite"},
	})
	if !ok {
		return nil, false
	}
	if idx == 1 {
		return items, true
	}
	out := items[:0]
	for _, it := range items {
		if fi, err := os.Stat(it.LocalPath); err == nil && !fi.IsDir() {
			continue
		}
		out = append(out, it)
	}
	return out, true
}

func partSize(it transfer.Item) int64 {
	if fi, err := os.Stat(it.LocalPath + transfer.PartSuffix); err == nil {
		return fi.Size()
	}
	return 0
}

// extractableAfter says whether items[i] completes an archive: a plain
// .zip/.7z/.rar, or the last downloaded volume of a multi-volume set
// (.7z.001, .part1.rar, .rar + .r00). It returns the volume to unpack from.
func extractableAfter(items []transfer.Item, i int) (string, bool) {
	first, key, ok := archive.Volume(items[i].LocalPath)
	if !ok {
		return "", false
	}
	for j := i + 1; j < len(items); j++ {
		if _, k, ok := archive.Volume(items[j].LocalPath); ok && k == key {
			return "", false // more volumes still to come
		}
	}
	if _, err := os.Stat(first); err != nil {
		return "", false
	}
	return first, true
}

// downloadOne shows one file's progress. B asks whether to stop; the
// download keeps running while the question is on screen.
func (e *Env) downloadOne(dl *transfer.Downloader, it transfer.Item, idx, total int) error {
	release := e.Platform.InhibitSleep()
	defer release()
	p := transfer.NewProgress()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	var dlErr error
	go func() {
		defer close(finished)
		dlErr = dl.Download(ctx, it, p)
	}()

	status := uatomic.NewString(T("st_connecting"))
	stopStatus := make(chan struct{})
	defer close(stopStatus)
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			status.Store(statusText(p))
			select {
			case <-stopStatus:
				return
			case <-t.C:
			}
		}
	}()

	// Small files finish before a progress screen is worth showing (and
	// gabagool keeps each one up for ~350 ms after completion).
	select {
	case <-finished:
		return dlErr
	case <-time.After(300 * time.Millisecond):
	}
	msg := T("downloading_n_of_m", idx+1, total) + "\n" + it.Name()
	if e.watch(msg, p.Fraction, status, finished, cancel, T("stop_download_q")) {
		return context.Canceled
	}
	return dlErr
}

// watch shows msg with a progress bar until finished is closed. B asks
// stopQuestion; on "stop" it cancels and waits for the work to wind down,
// otherwise the progress screen comes back. It reports whether the work was
// cancelled (by the user or by an app quit).
func (e *Env) watch(msg string, progress *uatomic.Float64, status *uatomic.String, finished <-chan struct{}, cancel func(), stopQuestion string) bool {
	items := []gaba.FooterHelpItem{{ButtonName: "B", HelpText: T("cancel"), Group: gaba.FooterGroupLeft}}
	if status != nil {
		items = append(items, gaba.FooterHelpItem{ButtonName: constants.Download, HelpTextDynamic: status, Group: gaba.FooterGroupRight})
	}
	opts := gaba.ProcessMessageOptions{
		ShowProgressBar: true,
		Progress:        progress,
		CancelButton:    constants.VirtualButtonB,
		FooterHelpItems: items,
	}
	for {
		_, perr := gaba.ProcessMessage(msg, opts, func() (struct{}, error) {
			<-finished
			return struct{}{}, nil
		})
		if e.QuitRequested() {
			cancel()
			<-finished
			return true
		}
		if !gaba.IsCancelled(perr) {
			<-finished
			return false
		}
		select {
		case <-finished:
			return false
		default:
		}
		if confirm(stopQuestion, T("stop"), T("continue")) {
			cancel()
			_ = e.runTask(T("stopping"), false, func(context.Context) error {
				<-finished
				return nil
			})
			return true
		}
	}
}

// devThrottle reads DSFETCH_THROTTLE_KBPS (development aid, -dev only).
func (e *Env) devThrottle() int64 {
	if e.Platform == nil || !e.Platform.Dev {
		return 0
	}
	kbps, _ := strconv.ParseInt(os.Getenv("DSFETCH_THROTTLE_KBPS"), 10, 64)
	return kbps * 1024
}

func statusText(p *transfer.Progress) string {
	switch p.State() {
	case transfer.StateConnecting:
		return T("st_connecting")
	case transfer.StateRetryWait:
		left := time.Until(time.Unix(0, p.RetryAt.Load())).Round(time.Second)
		return T("st_retry", p.Attempt.Load(), p.Retries.Load(), int(max(left, 0)/time.Second))
	case transfer.StateFinishing, transfer.StateDone:
		return T("st_finishing")
	}
	done, total := p.Done.Load(), p.Total.Load()
	parts := []string{humanize.Bytes(done)}
	if total >= 0 {
		parts[0] += " / " + humanize.Bytes(total)
	}
	parts = append(parts, humanize.Speed(p.Speed.Load()))
	if eta := p.ETA(); eta >= 0 {
		parts = append(parts, humanize.Duration(eta))
	}
	return strings.Join(parts, " · ")
}
