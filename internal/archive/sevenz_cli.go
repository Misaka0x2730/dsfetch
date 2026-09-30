package archive

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/atomic"

	"dsfetch/internal/platform"
)

// SevenZipCLI extracts .7z and .rar by running the official 7zz binary
// (arm64 build from 7-zip.org) as a subprocess. Listing, path validation,
// space checks, junk removal and flattening stay in Go; 7zz only decodes.
type SevenZipCLI struct {
	Options
}

// List implements Extractor.
func (s *SevenZipCLI) List(path string) ([]string, int64, error) {
	info, err := s.inspect(path)
	return info.Files, info.UnpackedSize, err
}

// inspect lists the archive with the Go reader of its format.
func (s *SevenZipCLI) inspect(path string) (Info, error) {
	if Kind(filepath.Base(path)) == "rar" {
		return inspectRar(path, s.Password)
	}
	return inspect7z(path, s.Password)
}

var percentRe = regexp.MustCompile(`(\d{1,3})%`)

var (
	// errCLIUnsupported: this 7zz cannot decode the archive's method (for
	// example Homebrew's build, which has no RAR codecs).
	errCLIUnsupported = errors.New("archive: 7zz cannot decode this archive")
	// errCLINames: some names are invalid on FAT (":", "?", ...), and 7zz
	// on Linux writes them as they are, which the card refuses.
	errCLINames = errors.New("archive: names need changing for the card")
)

// Extract implements Extractor. When this 7zz cannot decode the archive, or
// names must change for the card, the pure Go decoder takes over.
func (s *SevenZipCLI) Extract(ctx context.Context, path, dst string, progress *atomic.Float64) error {
	err := s.extract(ctx, path, dst, progress)
	if !errors.Is(err, errCLIUnsupported) && !errors.Is(err, errCLINames) {
		return err
	}
	slog.Info("unpacking with the built-in decoder instead of 7zz", "reason", err)
	builtin := s.Options
	builtin.SevenZipBinary = ""
	ex, ferr := For(path, builtin)
	if ferr != nil {
		return ferr
	}
	return ex.Extract(ctx, path, dst, progress)
}

func (s *SevenZipCLI) extract(ctx context.Context, path, dst string, progress *atomic.Float64) error {
	if progress == nil {
		progress = atomic.NewFloat64(0)
	}
	progress.Store(0)
	info, err := s.inspect(path) // rejects zip-slip names up front
	if err != nil {
		return err
	}
	if info.renamed {
		return errCLINames
	}
	if err := platform.CheckSpace(dst, info.UnpackedSize+(1<<20), info.LargestFile); err != nil {
		return err
	}
	tmp, cleanup, err := tempDirFor(dst, s.TempRoot)
	if err != nil {
		return err
	}
	defer cleanup()

	// -p with an empty value stops 7zz from prompting on a missing password.
	// -mmt=1: multithreaded LZMA2 decoding buffers whole blocks (500+ MB
	// peak RSS measured on a 345 MB ISO, 38 MB single-threaded) and the
	// console has 1 GB shared with the frontend.
	cmd := exec.CommandContext(ctx, s.SevenZipBinary, "x", "-y", "-bso0", "-bsp1", "-bse1", "-mmt=1",
		"-o"+tmp, "-p"+s.Password, "--", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("archive: start 7zz: %w", err)
	}

	var outMu sync.Mutex
	var output bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(io.TeeReader(stdout, lockedWriter{&outMu, &output}))
		sc.Split(scanProgress)
		for sc.Scan() {
			if m := percentRe.FindAllStringSubmatch(sc.Text(), -1); len(m) > 0 {
				if pct, err := strconv.Atoi(m[len(m)-1][1]); err == nil && pct <= 100 {
					progress.Store(float64(pct) / 100)
				}
			}
		}
	}()
	<-done
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if werr != nil {
		outMu.Lock()
		msg := output.String()
		outMu.Unlock()
		switch {
		case strings.Contains(msg, "Wrong password"):
			if s.Password == "" {
				return ErrPasswordRequired
			}
			return ErrWrongPassword
		case strings.Contains(msg, "Can't allocate required memory"), strings.Contains(msg, "Not enough memory"):
			return ErrOutOfMemory
		case strings.Contains(msg, "Unsupported Method"):
			return errCLIUnsupported
		}
		return fmt.Errorf("archive: 7zz failed: %v: %s", werr, lastLine(msg))
	}

	if err := removeJunkAndLinks(tmp); err != nil {
		return err
	}
	root, err := contentRoot(tmp, s.FlattenSingleDir)
	if err != nil {
		return err
	}
	if _, err := moveInto(root, dst); err != nil {
		return err
	}
	progress.Store(1)
	return nil
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.b.Len() < 64<<10 {
		w.b.Write(p)
	}
	return len(p), nil
}

// scanProgress splits on \n, \r and \b: 7zz redraws its progress line with
// backspaces instead of newlines.
func scanProgress(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		if c == '\n' || c == '\r' || c == '\b' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
