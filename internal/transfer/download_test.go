package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/afero"

	"dsfetch/internal/remote"
	"dsfetch/internal/remote/backends"
	"dsfetch/internal/remote/ftp/ftptest"
	"dsfetch/internal/remote/sftp/sftptest"
)

// fakeBackend serves in-memory files; each Open consumes one behaviour from
// the script ("ok", "fail@N", "eof@N", "stall@N", "noresume").
type fakeBackend struct {
	files  map[string][]byte
	mu     sync.Mutex
	script []string
	opens  []int64 // offsets requested
	closed int
}

func (f *fakeBackend) Connect(context.Context) error { return nil }
func (f *fakeBackend) List(context.Context, string) ([]remote.Entry, error) {
	return nil, nil
}
func (f *fakeBackend) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opens = append(f.opens, offset)
	step := "ok"
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	data, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", path, os.ErrNotExist)
	}
	if step == "noresume" && offset > 0 {
		return nil, remote.ErrResumeUnsupported
	}
	if step == "auth" {
		return nil, remote.ErrAuth
	}
	mode, atStr, found := strings.Cut(step, "@")
	var at int64
	if found {
		at, _ = strconv.ParseInt(atStr, 10, 64)
	} else {
		mode = ""
	}
	return &scriptedReader{ctx: ctx, data: data[offset:], mode: mode, left: at}, nil
}

type scriptedReader struct {
	ctx  context.Context
	data []byte
	mode string
	left int64
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.mode != "" && r.left <= 0 {
		switch r.mode {
		case "fail":
			return 0, errors.New("connection reset by peer")
		case "eof":
			return 0, io.EOF
		case "stall":
			<-r.ctx.Done() // like a dead TCP link, until the watchdog aborts
			return 0, r.ctx.Err()
		}
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if r.mode != "" && int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	r.left -= int64(n)
	return n, nil
}

func (r *scriptedReader) Close() error { return nil }

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func newDownloader(be remote.Backend) *Downloader {
	return &Downloader{
		Connect:      func(context.Context) (remote.Backend, error) { return be, nil },
		RetryDelays:  []time.Duration{10 * time.Millisecond},
		StallTimeout: 300 * time.Millisecond,
		BufferSize:   4096,
	}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if _, err := os.Stat(path + PartSuffix); !os.IsNotExist(err) {
		t.Fatalf(".part file left behind: %v", err)
	}
}

func TestDownloadPlain(t *testing.T) {
	data := payload(100_000)
	be := &fakeBackend{files: map[string][]byte{"/a.nds": data}}
	dst := filepath.Join(t.TempDir(), "NDS", "a.nds")
	p := NewProgress()
	if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/a.nds", Size: int64(len(data)), LocalPath: dst}, p); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
	if p.Done.Load() != int64(len(data)) || p.Fraction.Load() != 1 || p.State() != StateDone {
		t.Fatalf("progress: done=%d frac=%v state=%v", p.Done.Load(), p.Fraction.Load(), p.State())
	}
}

func TestResumeFromExistingPart(t *testing.T) {
	data := payload(50_000)
	be := &fakeBackend{files: map[string][]byte{"/a.gba": data}}
	dst := filepath.Join(t.TempDir(), "a.gba")
	os.WriteFile(dst+PartSuffix, data[:12_345], 0o644)

	if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/a.gba", Size: int64(len(data)), LocalPath: dst}, nil); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
	if len(be.opens) != 1 || be.opens[0] != 12_345 {
		t.Fatalf("opens = %v, want [12345]", be.opens)
	}
}

func TestRetriesAfterFailures(t *testing.T) {
	for _, step := range []string{"fail@20000", "eof@20000", "stall@20000"} {
		t.Run(step, func(t *testing.T) {
			data := payload(80_000)
			be := &fakeBackend{files: map[string][]byte{"/x.bin": data}, script: []string{step, "ok"}}
			dst := filepath.Join(t.TempDir(), "x.bin")
			p := NewProgress()
			if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/x.bin", Size: int64(len(data)), LocalPath: dst}, p); err != nil {
				t.Fatal(err)
			}
			checkFile(t, dst, data)
			if len(be.opens) != 2 || be.opens[1] != 20_000 {
				t.Fatalf("opens = %v, want second open at 20000", be.opens)
			}
			if p.Attempt.Load() != 1 {
				t.Fatalf("attempt = %d", p.Attempt.Load())
			}
			if be.closed == 0 {
				t.Fatal("connection was not dropped before retrying")
			}
		})
	}
}

func TestGivesUpAfterRetriesAndKeepsPart(t *testing.T) {
	data := payload(10_000)
	be := &fakeBackend{files: map[string][]byte{"/x": data},
		script: []string{"fail@1000", "fail@0", "fail@0", "fail@0", "fail@0", "fail@0", "ok"}}
	dst := filepath.Join(t.TempDir(), "x")
	p := NewProgress()
	err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/x", Size: int64(len(data)), LocalPath: dst}, p)
	if err == nil {
		t.Fatal("expected failure after 5 retries without data")
	}
	if len(be.opens) != 6 {
		t.Fatalf("opens = %d, want 6 (1 + 5 retries)", len(be.opens))
	}
	if p.Attempt.Load() != 5 || p.Retries.Load() != 5 {
		t.Fatalf("attempt %d of %d, want 5 of 5", p.Attempt.Load(), p.Retries.Load())
	}
	part, _ := os.ReadFile(dst + PartSuffix)
	if len(part) != 1000 || !bytes.Equal(part, data[:1000]) {
		t.Fatalf(".part has %d bytes, want the 1000 received", len(part))
	}
}

// Short drops on a long download: each attempt gets further, so the retries
// never run out.
func TestRetriesStartOverAfterData(t *testing.T) {
	data := payload(10_000)
	script := []string{}
	for i := 0; i < 8; i++ {
		script = append(script, "fail@1000")
	}
	be := &fakeBackend{files: map[string][]byte{"/x": data}, script: append(script, "ok")}
	dst := filepath.Join(t.TempDir(), "x")
	if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/x", Size: int64(len(data)), LocalPath: dst}, nil); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
	if len(be.opens) != 9 {
		t.Fatalf("opens = %d, want 9", len(be.opens))
	}
}

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	be := &fakeBackend{files: map[string][]byte{"/x": payload(10)}, script: []string{"auth"}}
	err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/x", Size: 10, LocalPath: filepath.Join(t.TempDir(), "x")}, nil)
	if !errors.Is(err, remote.ErrAuth) || len(be.opens) != 1 {
		t.Fatalf("err=%v opens=%d", err, len(be.opens))
	}

	be = &fakeBackend{files: map[string][]byte{}}
	err = newDownloader(be).Download(context.Background(), Item{RemotePath: "/missing", Size: 10, LocalPath: filepath.Join(t.TempDir(), "m")}, nil)
	if !errors.Is(err, os.ErrNotExist) || len(be.opens) != 1 {
		t.Fatalf("err=%v opens=%d", err, len(be.opens))
	}
}

func TestResumeUnsupportedRestarts(t *testing.T) {
	data := payload(30_000)
	be := &fakeBackend{files: map[string][]byte{"/r": data}, script: []string{"noresume", "ok"}}
	dst := filepath.Join(t.TempDir(), "r")
	os.WriteFile(dst+PartSuffix, data[:5000], 0o644)
	if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/r", Size: int64(len(data)), LocalPath: dst}, nil); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
	if be.opens[1] != 0 {
		t.Fatalf("second open at %d, want 0", be.opens[1])
	}
}

func TestCancelKeepsPart(t *testing.T) {
	data := payload(100_000)
	be := &fakeBackend{files: map[string][]byte{"/c": data}, script: []string{"stall@30000"}}
	dst := filepath.Join(t.TempDir(), "c")
	d := newDownloader(be)
	d.StallTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	err := d.Download(ctx, Item{RemotePath: "/c", Size: int64(len(data)), LocalPath: dst}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	part, _ := os.ReadFile(dst + PartSuffix)
	if len(part) != 30_000 {
		t.Fatalf(".part = %d bytes, want 30000", len(part))
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("final file must not exist after cancel")
	}
}

func TestNoSpace(t *testing.T) {
	be := &fakeBackend{files: map[string][]byte{"/huge": nil}}
	err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/huge", Size: 1 << 60, LocalPath: filepath.Join(t.TempDir(), "huge")}, nil)
	var ns *NoSpaceError
	if !errors.As(err, &ns) || len(be.opens) != 0 {
		t.Fatalf("err=%v opens=%d", err, len(be.opens))
	}
}

func TestUnknownSize(t *testing.T) {
	data := payload(5000)
	be := &fakeBackend{files: map[string][]byte{"/u": data}}
	dst := filepath.Join(t.TempDir(), "u")
	if err := newDownloader(be).Download(context.Background(), Item{RemotePath: "/u", Size: -1, LocalPath: dst}, nil); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
}

// End-to-end: a real FTP server drops the first transfer mid-way; the
// download reconnects and resumes with REST.
func TestFTPDropAndResume(t *testing.T) {
	data := payload(3 << 20)
	fs := afero.NewMemMapFs()
	afero.WriteFile(fs, "/roms/Покемон.nds", data, 0o644)
	srv := &ftptest.Server{FS: fs, Faults: ftptest.Faults{DropAfter: 1 << 20}}
	cfg := srv.Start(t)
	e2e(t, cfg, "/roms/Покемон.nds", data)
	if srv.Opens("/roms/Покемон.nds") != 2 {
		t.Fatalf("server opens = %d, want 2", srv.Opens("/roms/Покемон.nds"))
	}
}

// End-to-end over SFTP: the first transfer stalls (Wi-Fi gone); the
// watchdog aborts it and the retry resumes at the right offset.
func TestSFTPStallAndResume(t *testing.T) {
	root := t.TempDir()
	data := payload(3 << 20)
	os.MkdirAll(filepath.Join(root, "roms"), 0o755)
	os.WriteFile(filepath.Join(root, "roms", "игра.iso"), data, 0o644)
	srv := &sftptest.Server{Root: root, Faults: sftptest.Faults{StallAfter: 1 << 20}}
	cfg := srv.Start(t)
	e2e(t, cfg, "/roms/игра.iso", data)
	if srv.Opens("/roms/игра.iso") != 2 {
		t.Fatalf("server opens = %d, want 2", srv.Opens("/roms/игра.iso"))
	}
}

func TestSFTPDropAndResume(t *testing.T) {
	root := t.TempDir()
	data := payload(2 << 20)
	os.WriteFile(filepath.Join(root, "g.bin"), data, 0o644)
	srv := &sftptest.Server{Root: root, Faults: sftptest.Faults{DropAfter: 700_000}}
	e2e(t, srv.Start(t), "/g.bin", data)
}

func e2e(t *testing.T, cfg remote.Config, remotePath string, data []byte) {
	t.Helper()
	d := &Downloader{
		Connect: func(ctx context.Context) (remote.Backend, error) {
			be, err := backends.New(cfg)
			if err != nil {
				return nil, err
			}
			return be, be.Connect(ctx)
		},
		RetryDelays:  []time.Duration{50 * time.Millisecond},
		StallTimeout: 500 * time.Millisecond,
	}
	defer d.Close()
	dst := filepath.Join(t.TempDir(), filepath.Base(remotePath))
	p := NewProgress()
	if err := d.Download(context.Background(), Item{RemotePath: remotePath, Size: int64(len(data)), LocalPath: dst}, p); err != nil {
		t.Fatal(err)
	}
	checkFile(t, dst, data)
	if p.Attempt.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 retry", p.Attempt.Load())
	}
}
