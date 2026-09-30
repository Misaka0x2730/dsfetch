// Package transfer downloads remote files to the memory card: streaming into
// <name>.part, resuming from its size, retrying after Wi-Fi drops, checking
// free space and the FAT32 4 GiB limit, and renaming atomically at the end.
package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
)

// PartSuffix is appended to files while they download.
const PartSuffix = ".part"

// Item is one remote file and where it goes.
type Item struct {
	RemotePath string
	Size       int64 // -1 if the server did not report it
	LocalPath  string
}

// Name is the file name shown in the UI.
func (it Item) Name() string { return filepath.Base(it.LocalPath) }

var (
	ErrStalled    = errors.New("transfer: no data received, connection seems dead")
	ErrIncomplete = errors.New("transfer: connection closed before the file was complete")
)

// NoSpaceError and FAT32LimitError are shared with the archive package.
type (
	NoSpaceError    = platform.NoSpaceError
	FAT32LimitError = platform.FAT32LimitError
)

// Downloader downloads files one at a time over one reusable connection.
type Downloader struct {
	// Connect returns a new, connected backend. It is called for the first
	// file and again after a connection failure.
	Connect func(ctx context.Context) (remote.Backend, error)

	// Retries is how many failures in a row are retried (default 5): an
	// attempt that received data starts the count again, so a flaky Wi-Fi
	// does not use them up. With the default delays a connection may be
	// gone for about three minutes.
	Retries      int
	RetryDelays  []time.Duration // wait before each retry (default 3 s, 10 s, 30 s, 60 s, 60 s)
	StallTimeout time.Duration   // abort when no bytes arrive for this long (default 30 s)
	BufferSize   int             // default 256 KiB

	// FreeSpaceMargin is kept free on top of the file size (default 1 MiB).
	FreeSpaceMargin int64

	// RateLimit caps the speed in bytes/s (0 = unlimited). Only used to make
	// progress, cancel and resume observable during development.
	RateLimit int64

	backend remote.Backend
	buf     []byte
}

func (d *Downloader) retries() int {
	if d.Retries > 0 {
		return d.Retries
	}
	if d.Retries < 0 {
		return 0
	}
	return 5
}

func (d *Downloader) retryDelay(attempt int) time.Duration {
	delays := d.RetryDelays
	if len(delays) == 0 {
		delays = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	}
	if attempt-1 < len(delays) {
		return delays[attempt-1]
	}
	return delays[len(delays)-1]
}

func (d *Downloader) stallTimeout() time.Duration {
	if d.StallTimeout > 0 {
		return d.StallTimeout
	}
	return 30 * time.Second
}

// Close drops the connection.
func (d *Downloader) Close() {
	if d.backend != nil {
		_ = d.backend.Close()
		d.backend = nil
	}
}

// Preflight checks the target filesystem before any byte is transferred.
func (d *Downloader) Preflight(it Item) error {
	if it.Size < 0 {
		return nil
	}
	have := fileSize(it.LocalPath + PartSuffix)
	if have > it.Size {
		have = 0
	}
	margin := d.FreeSpaceMargin
	if margin == 0 {
		margin = 1 << 20
	}
	return platform.CheckSpace(filepath.Dir(it.LocalPath), it.Size-have+margin, it.Size)
}

// Download fetches one item. On cancellation the .part file is kept so a
// later download of the same file resumes.
func (d *Downloader) Download(ctx context.Context, it Item, p *Progress) error {
	if p == nil {
		p = NewProgress()
	}
	part := it.LocalPath + PartSuffix
	if err := os.MkdirAll(filepath.Dir(it.LocalPath), 0o755); err != nil {
		return err
	}
	if it.Size >= 0 && fileSize(part) > it.Size {
		_ = os.Remove(part) // stale partial of a different file
	}
	if err := d.Preflight(it); err != nil {
		return err
	}
	p.start(it.Name(), it.Size, fileSize(part))
	p.Retries.Store(int32(d.retries()))

	for attempt := 0; ; attempt++ {
		before := fileSize(part)
		err := d.attempt(ctx, it, part, p)
		if err == nil {
			break
		}
		if fileSize(part) > before {
			attempt = 0 // the connection worked again: a new streak
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, remote.ErrResumeUnsupported) {
			slog.Info("server cannot resume, restarting from zero", "file", it.RemotePath)
			_ = os.Remove(part)
			p.setDone(0)
			continue
		}
		if permanent(err) || attempt >= d.retries() {
			return err
		}
		slog.Warn("download interrupted, will retry", "file", it.RemotePath, "attempt", attempt+1, "err", err)
		d.Close()
		wait := d.retryDelay(attempt + 1)
		p.Attempt.Store(int32(attempt + 1))
		p.setRetry(err, time.Now().Add(wait))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	p.setState(StateFinishing)
	if it.Size >= 0 {
		if got := fileSize(part); got != it.Size {
			return fmt.Errorf("transfer: size mismatch after download: got %d, want %d", got, it.Size)
		}
	}
	if err := os.Rename(part, it.LocalPath); err != nil {
		return err
	}
	p.setState(StateDone)
	return nil
}

func (d *Downloader) attempt(ctx context.Context, it Item, part string, p *Progress) error {
	p.setState(StateConnecting)
	if d.backend == nil {
		be, err := d.Connect(ctx)
		if err != nil {
			return err
		}
		d.backend = be
	}

	offset := fileSize(part)
	if it.Size >= 0 && offset == it.Size && offset > 0 {
		return nil // already complete (e.g. crashed right before rename)
	}
	p.setDone(offset)
	slog.Info("transfer attempt", "file", it.RemotePath, "offset", offset, "size", it.Size, "attempt", p.Attempt.Load())

	// The watchdog cancels this context when no bytes arrive in time; the
	// backend then closes its connection, which unblocks the read.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	var counter atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	go d.watchdog(stop, &counter, &stalled, cancel)

	r, err := d.backend.Open(rctx, it.RemotePath, offset)
	if err != nil {
		if stalled.Load() {
			return ErrStalled
		}
		return err
	}

	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		_ = r.Close()
		return err
	}
	if d.buf == nil {
		size := d.BufferSize
		if size <= 0 {
			size = 256 << 10
		}
		d.buf = make([]byte, size)
	}
	bw := bufio.NewWriterSize(f, len(d.buf))
	pw := &progressWriter{ctx: rctx, w: bw, p: p, counter: &counter, rate: d.RateLimit, start: time.Now()}

	p.setState(StateDownloading)
	n, copyErr := io.CopyBuffer(pw, r, d.buf)
	closeErr := r.Close()

	// Whatever arrived is valid data: keep it for resuming.
	flushErr := bw.Flush()
	syncErr := f.Sync()
	fileErr := f.Close()

	switch {
	case stalled.Load():
		return ErrStalled
	case ctx.Err() != nil:
		return ctx.Err()
	case copyErr != nil:
		if pw.writeErr != nil {
			return pw.writeErr // local disk problem
		}
		return copyErr
	case flushErr != nil:
		return flushErr
	case syncErr != nil:
		return syncErr
	case fileErr != nil:
		return fileErr
	}
	if it.Size >= 0 && offset+n < it.Size {
		return ErrIncomplete
	}
	if closeErr != nil && it.Size < 0 {
		// Without a known size, a failed transfer status is the only
		// signal that the file is truncated.
		return closeErr
	}
	return nil
}

func (d *Downloader) watchdog(stop <-chan struct{}, counter *atomic.Int64, stalled *atomic.Bool, cancel func()) {
	timeout := d.stallTimeout()
	tick := timeout / 10
	if tick > time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	last := counter.Load()
	lastChange := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if v := counter.Load(); v != last {
				last, lastChange = v, time.Now()
				continue
			}
			if time.Since(lastChange) >= timeout {
				stalled.Store(true)
				cancel()
				return
			}
		}
	}
}

type progressWriter struct {
	// ctx stops the copy at once on cancel: pkg/sftp's concurrent WriteTo
	// would otherwise keep flushing megabytes of read-ahead into the file.
	ctx      context.Context
	w        io.Writer
	p        *Progress
	counter  *atomic.Int64
	writeErr error

	rate    int64
	start   time.Time
	written int64
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	if err := pw.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pw.w.Write(b)
	if n > 0 {
		pw.counter.Add(int64(n))
		pw.p.add(int64(n))
		if pw.rate > 0 {
			pw.written += int64(n)
			due := pw.start.Add(time.Duration(float64(pw.written) / float64(pw.rate) * float64(time.Second)))
			if wait := time.Until(due); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	if err != nil {
		pw.writeErr = err
	}
	return n, err
}

// permanent reports errors that retrying will not fix.
func permanent(err error) bool {
	if isNetwork(err) {
		return false
	}
	var mismatch *remote.HostKeyMismatchError
	var noSpace *NoSpaceError
	var fat *FAT32LimitError
	var pathErr *fs.PathError
	var tp *textproto.Error
	switch {
	case errors.Is(err, remote.ErrAuth),
		errors.As(err, &mismatch),
		errors.As(err, &noSpace),
		errors.As(err, &fat),
		errors.Is(err, fs.ErrNotExist),
		errors.Is(err, fs.ErrPermission):
		return true
	case errors.As(err, &tp):
		return tp.Code == 550 || tp.Code == 553 // no such file / not allowed
	case errors.As(err, &pathErr):
		// Local file errors (read-only card, disk full) - not the network.
		return true
	}
	return false
}

// isNetwork reports connection-level failures, which are worth a retry even
// when a library wraps them in *fs.PathError (go-smb2 does).
func isNetwork(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, ErrStalled) ||
		errors.Is(err, ErrIncomplete)
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
