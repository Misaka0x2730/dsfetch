package ui

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	uatomic "go.uber.org/atomic"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
	"dsfetch/internal/store"
)

// memoryReserve is kept free for the app and the system while decoding.
const memoryReserve = 64 << 20

// extractFlow unpacks an archive into dst: password prompt, RAM check,
// progress screen, then the delete-archive policy. ok is true when the
// archive was unpacked; a user cancel or skip returns ok=false, err=nil.
func (e *Env) extractFlow(archivePath, dst string) (ok bool, err error) {
	name := filepath.Base(archivePath)
	opts := archive.Options{FlattenSingleDir: e.Settings.FlattenSingleDir, SevenZipBinary: e.sevenZipBinary()}
	if card, ok := cardFor(e.Platform.AvailableCards(), dst); ok {
		opts.TempRoot = card.Path
	}

	var inf archive.Info
	for {
		err := e.runTask(T("checking_archive"), false, func(context.Context) error {
			var err error
			inf, err = archive.Inspect(archivePath, opts)
			return err
		})
		if e.QuitRequested() {
			return false, nil
		}
		if errors.Is(err, archive.ErrPasswordRequired) || errors.Is(err, archive.ErrWrongPassword) {
			pw, ok := e.askArchivePassword(name, errors.Is(err, archive.ErrWrongPassword))
			if !ok {
				return false, nil
			}
			opts.Password = pw
			continue
		}
		if err != nil {
			info(errorMessage(T("err_unpack", name), err))
			return false, err
		}
		break
	}

	// LZMA needs its whole dictionary in RAM; the console has 1 GB shared
	// with the frontend.
	if inf.DecoderMemory > 0 {
		if avail, ok := platform.MemAvailable(); ok && uint64(inf.DecoderMemory)+memoryReserve > avail {
			if !confirm(T("warn_memory", humanize.Bytes(inf.DecoderMemory), humanize.Bytes(int64(avail))), T("unpack_anyway"), T("skip")) {
				return false, nil
			}
		}
	}

	for {
		start := time.Now()
		err := e.runExtract(archivePath, dst, opts)
		if e.QuitRequested() || errors.Is(err, context.Canceled) {
			return false, nil
		}
		if errors.Is(err, archive.ErrWrongPassword) || errors.Is(err, archive.ErrPasswordRequired) {
			pw, ok := e.askArchivePassword(name, true)
			if !ok {
				return false, nil
			}
			opts.Password = pw
			continue
		}
		if err != nil {
			slog.Error("unpack failed", "archive", archivePath, "err", err)
			info(errorMessage(T("err_unpack", name), err))
			return false, err
		}
		slog.Info("unpacked", "archive", archivePath, "to", dst, "bytes", inf.UnpackedSize,
			"seconds", time.Since(start).Seconds(), "engine", engineName(opts))
		break
	}
	e.Platform.RequestRescan()
	e.maybeDeleteArchive(archivePath)
	return true, nil
}

func engineName(o archive.Options) string {
	if o.SevenZipBinary != "" {
		return "7zz"
	}
	return "builtin"
}

func (e *Env) runExtract(path, dst string, opts archive.Options) error {
	ex, err := archive.For(path, opts)
	if err != nil {
		return err
	}
	progress := uatomic.NewFloat64(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	var exErr error
	go func() {
		defer close(finished)
		exErr = ex.Extract(ctx, path, dst, progress)
	}()
	release := e.Platform.InhibitSleep()
	defer release()
	select {
	case <-finished:
		return exErr
	case <-time.After(300 * time.Millisecond):
	}
	if e.watch(T("unpacking")+"\n"+filepath.Base(path), progress, nil, finished, cancel, T("stop_unpack_q")) {
		return context.Canceled
	}
	return exErr
}

func (e *Env) maybeDeleteArchive(path string) {
	switch e.Settings.DeleteArchive {
	case store.Never:
		return
	case store.Ask:
		if !confirm(T("delete_archive_q", filepath.Base(path)), T("delete"), T("keep")) {
			return
		}
	}
	for _, v := range archive.Volumes(path) {
		if err := os.Remove(v); err != nil {
			slog.Warn("delete archive", "file", v, "err", err)
		}
	}
}

// askArchivePassword shows a password form for an encrypted 7z.
func (e *Env) askArchivePassword(name string, wrong bool) (string, bool) {
	if wrong {
		info(T("err_wrong_password"))
	}
	const (
		rowPassword = iota
		rowGo
	)
	items := []gaba.ItemWithOptions{
		{Item: gaba.MenuItem{Text: T("f_password")}, Options: []gaba.Option{kbOption("", gaba.KeyboardLayoutGeneral, true)}},
		{Item: gaba.MenuItem{Text: T("unpack")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}},
	}
	settings := gaba.OptionListSettings{
		FooterHelpItems: footer(btn("B", T("cancel")), btn("A", T("change")), startBtn(T("unpack"))),
		HelpExitText:    T("help_exit"),
	}
	for {
		res, err := gaba.OptionsList(T("archive_password_for", name), settings, items)
		if err != nil || e.QuitRequested() {
			return "", false
		}
		if res.Action != gaba.ListActionConfirmed && res.Selected != rowGo {
			continue
		}
		pw, _ := items[rowPassword].Options[items[rowPassword].SelectedOption].Value.(string)
		return pw, true
	}
}
