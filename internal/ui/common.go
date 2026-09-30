package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
)

// Glyphs from the Nerd Font bundled with gabagool.
const (
	iconFolder  = ""
	iconFile    = ""
	iconArchive = ""
	iconLink    = ""
	iconServer  = ""
	iconCog     = ""
	iconCard    = ""
	iconUp      = ""
)

func footer(items ...gaba.FooterHelpItem) []gaba.FooterHelpItem { return items }

func btn(name, text string) gaba.FooterHelpItem {
	return gaba.FooterHelpItem{ButtonName: name, HelpText: text}
}

// startBtn labels the Start button in text: gabagool's play-triangle glyph
// was not recognisable as Start.
func startBtn(text string) gaba.FooterHelpItem {
	return gaba.FooterHelpItem{ButtonName: "START", HelpText: text}
}

// selectBtn labels the Select button in text: gabagool's Select glyph
// renders as a bare dash in the bundled font.
func selectBtn(text string) gaba.FooterHelpItem {
	return gaba.FooterHelpItem{ButtonName: "SELECT", HelpText: text}
}

// info shows a message until A or B is pressed.
func info(msg string) {
	_, _ = gaba.ConfirmationMessage(msg, footer(btn("A", T("ok"))), gaba.MessageOptions{})
}

// ReportSetAside says which damaged data files were renamed to .bad at
// start, so that DSFetch runs without them.
func ReportSetAside(names []string) {
	if len(names) > 0 {
		info(T("files_set_aside", strings.Join(names, "\n")))
	}
}

// confirm asks a yes/no question; A = yes.
func confirm(msg, yes, no string) bool {
	res, err := gaba.ConfirmationMessage(msg, footer(btn("B", no), btn("A", yes)), gaba.MessageOptions{})
	return err == nil && res != nil && res.Confirmed
}

// choose shows a selection dialog and returns the chosen value index.
func choose(msg string, options []gaba.SelectionOption) (int, bool) {
	res, err := gaba.SelectionMessage(msg, options, footer(btn("B", T("cancel")), btn("A", T("select"))), gaba.SelectionMessageSettings{})
	if err != nil || res == nil {
		return -1, false
	}
	return res.SelectedIndex, true
}

// runTask runs fn in the background. A spinner with msg appears only when
// fn takes longer than a moment, so quick listings do not flash a screen.
// With cancellable, B cancels fn's context; the result is then
// context.Canceled.
func (e *Env) runTask(msg string, cancellable bool, fn func(ctx context.Context) error) error {
	release := e.Platform.InhibitSleep() // work in progress: no lid sleep
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = fn(ctx)
	}()
	select {
	case <-done:
		return err
	case <-time.After(250 * time.Millisecond):
	}
	opts := gaba.ProcessMessageOptions{}
	if cancellable {
		opts.CancelButton = constants.VirtualButtonB
		opts.FooterHelpItems = footer(btn("B", T("cancel")))
	}
	_, perr := gaba.ProcessMessage(msg, opts, func() (struct{}, error) {
		<-done
		return struct{}{}, nil
	})
	if gaba.IsCancelled(perr) || e.QuitRequested() {
		cancel()
		<-done
		return context.Canceled
	}
	<-done
	return err
}

// describe turns an error into a sentence for the user.
func describe(err error) string {
	var mismatch *remote.HostKeyMismatchError
	var noSpace *platform.NoSpaceError
	var fat *platform.FAT32LimitError
	var unsafe *archive.UnsafePathError
	var method *archive.UnsupportedMethodError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, remote.ErrAuth):
		return T("err_auth")
	case errors.As(err, &mismatch):
		return T("err_hostkey")
	case errors.As(err, &noSpace):
		return T("err_nospace", humanize.Bytes(noSpace.Need), humanize.Bytes(noSpace.Free))
	case errors.As(err, &fat):
		return T("err_fat32", humanize.Bytes(fat.Size))
	case errors.Is(err, archive.ErrEncryptedZip):
		return T("err_zip_encrypted")
	case errors.Is(err, archive.ErrWrongPassword):
		return T("err_wrong_password")
	case errors.Is(err, archive.ErrPasswordRequired):
		return T("err_password_required")
	case errors.As(err, &unsafe):
		return T("err_unsafe_archive", unsafe.Name)
	case errors.As(err, &method):
		return T("err_zip_method")
	case errors.Is(err, archive.ErrUnsupported):
		return T("err_not_archive")
	case errors.Is(err, archive.ErrOutOfMemory):
		return T("err_out_of_memory")
	case errors.As(err, &dnsErr):
		return T("err_dns", dnsErr.Name)
	case errors.Is(err, syscall.ECONNREFUSED):
		return T("err_refused")
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return T("err_unreachable")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return T("err_timeout")
	case errors.Is(err, syscall.EROFS):
		return T("err_readonly")
	}
	msg := err.Error()
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return msg
}

// errorMessage formats "<what failed>\n\n<why>".
func errorMessage(what string, err error) string {
	return fmt.Sprintf("%s\n\n%s", what, describe(err))
}

func trimSlash(s string) string { return strings.Trim(s, "/") }
