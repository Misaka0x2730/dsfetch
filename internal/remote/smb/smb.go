// Package smb implements remote.Backend over SMB2/3 (cloudsoda/go-smb2,
// NTLMv2 or guest). The root "/" lists the server's disk shares; paths below
// are "/<share>/<path in share>".
package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	smb2 "github.com/cloudsoda/go-smb2"

	"dsfetch/internal/remote"
)

// NTSTATUS codes that mean "the login was rejected".
var authStatus = map[uint32]bool{
	0xC000006D: true, // STATUS_LOGON_FAILURE
	0xC000006E: true, // STATUS_ACCOUNT_RESTRICTION
	0xC0000070: true, // STATUS_INVALID_WORKSTATION
	0xC0000071: true, // STATUS_PASSWORD_EXPIRED
	0xC0000072: true, // STATUS_ACCOUNT_DISABLED
	0xC0000193: true, // STATUS_ACCOUNT_EXPIRED
	0xC0000224: true, // STATUS_PASSWORD_MUST_CHANGE
	0xC0000234: true, // STATUS_ACCOUNT_LOCKED_OUT
}

// Backend is one SMB session.
type Backend struct {
	cfg remote.Config

	mu     sync.Mutex
	raw    net.Conn
	sess   *smb2.Session
	shares map[string]*smb2.Share
	stop   chan struct{}
}

// New returns an unconnected backend.
func New(cfg remote.Config) *Backend {
	return &Backend{cfg: cfg}
}

// Connect dials and authenticates.
func (b *Backend) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess != nil {
		return nil
	}
	timeout := b.cfg.DialTimeoutOrDefault()
	addr := b.cfg.Address(445)
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smb: connect: %w", err)
	}
	_ = raw.SetDeadline(time.Now().Add(2 * timeout))
	stopAbort := remote.AbortOnCancel(ctx, func() { raw.Close() })
	defer stopAbort()

	user, pass := b.cfg.User, b.cfg.Password
	if b.cfg.Anonymous {
		// Samba maps unknown users to guest with "map to guest = bad user";
		// Windows and macOS accept the Guest account with an empty password.
		user, pass = "guest", ""
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: pass, Domain: b.cfg.Domain}}
	sess, err := d.DialConn(ctx, raw, b.cfg.Host)
	if err != nil {
		raw.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var re *smb2.ResponseError
		if errors.As(err, &re) && authStatus[re.Code] {
			return fmt.Errorf("%w: %v", remote.ErrAuth, err)
		}
		return fmt.Errorf("smb: session setup: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	b.raw, b.sess, b.shares = raw, sess, map[string]*smb2.Share{}
	b.startKeepalive()
	return nil
}

func (b *Backend) startKeepalive() {
	interval := b.cfg.KeepaliveOrDefault()
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	b.stop = stop
	sess, raw := b.sess, b.raw
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				reply := make(chan error, 1)
				go func() { reply <- sess.Echo() }()
				select {
				case <-reply:
				case <-time.After(15 * time.Second):
					raw.Close()
					return
				case <-stop:
					return
				}
			}
		}
	}()
}

// split turns "/share/a/b" into ("share", "a/b").
func split(p string) (share, sub string) {
	p = strings.TrimPrefix(remote.Clean(p), "/")
	if p == "" {
		return "", ""
	}
	share, sub, _ = strings.Cut(p, "/")
	return share, sub
}

func (b *Backend) session() (*smb2.Session, net.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess == nil {
		return nil, nil, remote.ErrNotConnected
	}
	return b.sess, b.raw, nil
}

func (b *Backend) mount(ctx context.Context, name string) (*smb2.Share, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess == nil {
		return nil, remote.ErrNotConnected
	}
	if sh, ok := b.shares[strings.ToLower(name)]; ok {
		return sh, nil
	}
	sh, err := b.sess.WithContext(ctx).Mount(name)
	if err != nil {
		return nil, fmt.Errorf("smb: open share %s: %w", name, err)
	}
	b.shares[strings.ToLower(name)] = sh
	return sh, nil
}

// List shows shares at "/" and directory contents below.
func (b *Backend) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	sess, raw, err := b.session()
	if err != nil {
		return nil, err
	}
	stop := remote.AbortOnCancel(ctx, func() { raw.Close() })
	defer stop()

	share, sub := split(dir)
	if share == "" {
		infos, err := sess.WithContext(ctx).ListShares()
		if err != nil {
			return nil, fmt.Errorf("smb: list shares: %w", err)
		}
		var out []remote.Entry
		for _, s := range infos {
			if s.IsSpecial() || s.Type() != smb2.ShareTypeDiskTree || strings.HasSuffix(s.Name, "$") {
				continue
			}
			out = append(out, remote.Entry{Name: s.Name, IsDir: true})
		}
		return out, nil
	}

	sh, err := b.mount(ctx, share)
	if err != nil {
		return nil, err
	}
	if sub == "" {
		sub = "."
	}
	infos, err := sh.WithContext(ctx).ReadDir(sub)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("smb: list %s: %w", dir, err)
	}
	out := make([]remote.Entry, 0, len(infos))
	for _, fi := range infos {
		if fi.Name() == "." || fi.Name() == ".." {
			continue
		}
		out = append(out, remote.Entry{Name: fi.Name(), Size: fi.Size(), IsDir: fi.IsDir(), ModTime: fi.ModTime()})
	}
	return out, nil
}

type fileReader struct {
	*smb2.File
	stop func() bool
}

func (r *fileReader) Close() error {
	r.stop()
	return r.File.Close()
}

// Open returns the file positioned at offset.
func (b *Backend) Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	_, raw, err := b.session()
	if err != nil {
		return nil, err
	}
	share, sub := split(path)
	if share == "" || sub == "" {
		return nil, fmt.Errorf("smb: %s is not a file", path)
	}
	sh, err := b.mount(ctx, share)
	if err != nil {
		return nil, err
	}
	stop := remote.AbortOnCancel(ctx, func() { raw.Close() })
	f, err := sh.WithContext(ctx).Open(sub)
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("smb: open %s: %w", path, err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			stop()
			f.Close()
			return nil, fmt.Errorf("smb: seek: %w", err)
		}
	}
	return &fileReader{File: f, stop: stop}, nil
}

// Close unmounts shares and logs off.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
	for _, sh := range b.shares {
		_ = sh.Umount()
	}
	b.shares = nil
	if b.sess != nil {
		_ = b.sess.Logoff()
		b.sess = nil
	}
	if b.raw != nil {
		_ = b.raw.Close()
		b.raw = nil
	}
	return nil
}
