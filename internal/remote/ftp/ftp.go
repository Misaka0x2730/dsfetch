// Package ftp implements remote.Backend over FTP/FTPS using jlaffaye/ftp.
//
// Data connections are passive: EPSV first, PASV if the server refuses (the
// library falls back on its own). Active mode is not available in the
// library. Listings use MLSD when the server advertises it and fall back to
// LIST if MLSD fails at runtime. Old servers that send Cyrillic names in
// CP1251/CP866 are handled by transcoding paths in both directions.
package ftp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"

	jftp "github.com/jlaffaye/ftp"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"

	"dsfetch/internal/remote"
)

// Backend is one FTP control connection.
type Backend struct {
	cfg remote.Config
	enc encoding.Encoding // nil = UTF-8

	mu          sync.Mutex // serialises commands on the control connection
	conn        *jftp.ServerConn
	disableMLSD bool

	connsMu sync.Mutex
	conns   []net.Conn // raw TCP conns (control + data) for abort

	fpMu        sync.Mutex
	fingerprint string

	stopKeepalive chan struct{}
}

// New returns an unconnected backend.
func New(cfg remote.Config) *Backend {
	b := &Backend{cfg: cfg}
	switch strings.ToLower(cfg.Encoding) {
	case "cp1251", "windows-1251":
		b.enc = charmap.Windows1251
	case "cp866", "ibm866":
		b.enc = charmap.CodePage866
	}
	return b
}

// Fingerprint of the FTPS certificate ("" for plain FTP).
func (b *Backend) Fingerprint() string {
	b.fpMu.Lock()
	defer b.fpMu.Unlock()
	return b.fingerprint
}

// Connect dials, logs in and starts the keepalive.
func (b *Backend) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil
	}
	c, err := b.dial(ctx)
	if err != nil {
		return err
	}
	b.conn = c
	b.startKeepalive()
	return nil
}

func (b *Backend) dial(ctx context.Context) (*jftp.ServerConn, error) {
	timeout := b.cfg.DialTimeoutOrDefault()
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var tlsConf *tls.Config
	if b.cfg.TLS {
		tlsConf = &tls.Config{
			// Home NAS certificates are self-signed; identity is pinned
			// by fingerprint instead (trust on first use).
			InsecureSkipVerify: true,
			// Sessions are cached per ServerName (else per host:port), and
			// vsftpd/ProFTPD refuse data connections that do not resume
			// the control connection's session: one name for all of them.
			ServerName:         b.cfg.Host,
			ClientSessionCache: tls.NewLRUClientSessionCache(8),
			VerifyConnection:   b.verifyCert,
		}
	}

	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	first := true
	dialFunc := func(network, address string) (net.Conn, error) {
		// The first dial is the control connection; the rest are data
		// connections, which the library does not wrap in TLS when a
		// custom dial func is used.
		dctx := dialCtx
		if !first {
			var c context.CancelFunc
			dctx, c = context.WithTimeout(context.Background(), timeout)
			defer c()
		}
		raw, err := dialer.DialContext(dctx, network, address)
		if err != nil {
			return nil, err
		}
		b.track(raw)
		if first {
			first = false
			return raw, nil
		}
		if tlsConf != nil {
			return tls.Client(raw, tlsConf), nil
		}
		return raw, nil
	}

	opts := []jftp.DialOption{
		jftp.DialWithDialFunc(dialFunc),
		jftp.DialWithDisabledMLSD(b.disableMLSD),
	}
	if b.enc != nil {
		opts = append(opts, jftp.DialWithDisabledUTF8(true))
	}
	if tlsConf != nil {
		opts = append(opts, jftp.DialWithExplicitTLS(tlsConf))
	}

	stop := remote.AbortOnCancel(dialCtx, b.abortConns)
	defer stop()

	c, err := jftp.Dial(b.cfg.Address(21), opts...)
	if err != nil {
		if dialCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("ftp: connect timeout: %w", err)
		}
		var mismatch *remote.HostKeyMismatchError
		if errors.As(err, &mismatch) {
			return nil, mismatch
		}
		return nil, fmt.Errorf("ftp: connect: %w", err)
	}

	user, pass := b.cfg.User, b.cfg.Password
	if b.cfg.Anonymous || user == "" {
		user, pass = "anonymous", "dsfetch@"
	}
	if err := c.Login(user, pass); err != nil {
		_ = c.Quit()
		b.abortConns()
		return nil, mapLoginError(err)
	}
	return c, nil
}

func (b *Backend) verifyCert(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("ftp: server sent no certificate")
	}
	fp := remote.Fingerprint(cs.PeerCertificates[0].Raw)
	b.fpMu.Lock()
	b.fingerprint = fp
	b.fpMu.Unlock()
	return remote.CheckFingerprint(b.cfg.KnownFingerprint, fp)
}

func mapLoginError(err error) error {
	// With explicit TLS the handshake (and so the certificate check) runs
	// lazily on the first command after AUTH TLS, i.e. inside Login.
	var mismatch *remote.HostKeyMismatchError
	if errors.As(err, &mismatch) {
		return mismatch
	}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		if tp.Code == 530 || tp.Code == 430 || tp.Code == 331 || tp.Code == 332 {
			return fmt.Errorf("%w: %s", remote.ErrAuth, strings.TrimSpace(tp.Msg))
		}
		return fmt.Errorf("ftp: login: %w", err)
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) {
		return fmt.Errorf("ftp: login: %w", err)
	}
	// USER rejected outright (e.g. anonymous disabled) comes back as a bare
	// message without a code.
	return fmt.Errorf("%w: %v", remote.ErrAuth, err)
}

func (b *Backend) track(c net.Conn) {
	b.connsMu.Lock()
	defer b.connsMu.Unlock()
	// Forget data connections that are long closed; keep the list short.
	if len(b.conns) > 16 {
		b.conns = append(b.conns[:1], b.conns[len(b.conns)-4:]...)
	}
	b.conns = append(b.conns, c)
}

// abortConns closes every raw connection, unblocking any stuck read.
func (b *Backend) abortConns() {
	b.connsMu.Lock()
	defer b.connsMu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
}

func (b *Backend) startKeepalive() {
	interval := b.cfg.KeepaliveOrDefault()
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	b.stopKeepalive = stop
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// Skip while a command or a download holds the connection.
				if !b.mu.TryLock() {
					continue
				}
				if b.conn != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					stopAbort := remote.AbortOnCancel(ctx, b.abortConns)
					_ = b.conn.NoOp()
					stopAbort()
					cancel()
				}
				b.mu.Unlock()
			}
		}
	}()
}

// List returns the entries of dir (without "." and "..").
func (b *Backend) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil, remote.ErrNotConnected
	}
	entries, err := b.listLocked(ctx, dir)
	if err != nil && !b.disableMLSD && isCommandRejected(err) && ctx.Err() == nil {
		// MLSD advertised but broken: reconnect using LIST.
		b.disableMLSD = true
		b.closeLocked()
		c, derr := b.dial(ctx)
		if derr != nil {
			return nil, derr
		}
		b.conn = c
		b.startKeepalive()
		entries, err = b.listLocked(ctx, dir)
	}
	return entries, err
}

func (b *Backend) listLocked(ctx context.Context, dir string) ([]remote.Entry, error) {
	stop := remote.AbortOnCancel(ctx, b.abortConns)
	defer stop()

	raw, err := b.conn.List(b.toServer(remote.Clean(dir)))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("ftp: list %s: %w", dir, err)
	}
	out := make([]remote.Entry, 0, len(raw))
	var links []int
	for _, e := range raw {
		name := b.fromServer(e.Name)
		if name == "." || name == ".." || name == "" {
			continue
		}
		// Some servers return full paths in LIST output.
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if e.Type == jftp.EntryTypeLink {
			links = append(links, len(out))
		}
		out = append(out, remote.Entry{
			Name:    name,
			Size:    int64(e.Size),
			IsDir:   e.Type == jftp.EntryTypeFolder,
			IsLink:  e.Type == jftp.EntryTypeLink,
			ModTime: e.Time,
		})
	}
	if len(links) > 0 {
		b.resolveLinks(dir, out, links)
	}
	return out, nil
}

// resolveLinks decides whether symlinks point at folders: only a folder can
// be entered with CWD (LIST succeeds on files too, so it cannot tell).
func (b *Backend) resolveLinks(dir string, entries []remote.Entry, idx []int) {
	pwd, err := b.conn.CurrentDir()
	if err != nil {
		return
	}
	for _, i := range idx {
		if b.conn.ChangeDir(b.toServer(remote.Join(dir, entries[i].Name))) == nil {
			entries[i].IsDir = true
			entries[i].IsLink = false
			_ = b.conn.ChangeDir(pwd)
		}
	}
}

func isCommandRejected(err error) bool {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code == 500 || tp.Code == 501 || tp.Code == 502 || tp.Code == 504
	}
	return false
}

// Open starts RETR at offset. The control connection stays locked until the
// returned reader is closed.
func (b *Backend) Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	b.mu.Lock()
	if b.conn == nil {
		b.mu.Unlock()
		return nil, remote.ErrNotConnected
	}
	stop := remote.AbortOnCancel(ctx, b.abortConns)
	resp, err := b.conn.RetrFrom(b.toServer(remote.Clean(path)), uint64(offset))
	if err != nil {
		stop()
		b.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var tp *textproto.Error
		if offset > 0 && errors.As(err, &tp) && (tp.Code == 500 || tp.Code == 502 || tp.Code == 504 || tp.Code == 554) {
			return nil, fmt.Errorf("%w: %v", remote.ErrResumeUnsupported, err)
		}
		return nil, fmt.Errorf("ftp: retr %s: %w", path, err)
	}
	var once sync.Once
	return remote.ReadCloserFunc{
		Reader: resp,
		CloseFunc: func() error {
			var cerr error
			once.Do(func() {
				cerr = resp.Close()
				stop()
				b.mu.Unlock()
			})
			return cerr
		},
	}, nil
}

// Close logs out and closes all connections.
func (b *Backend) Close() error {
	// Unblock anything in flight first so Lock cannot hang on a dead link.
	b.abortConns()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
	return nil
}

func (b *Backend) closeLocked() {
	if b.stopKeepalive != nil {
		close(b.stopKeepalive)
		b.stopKeepalive = nil
	}
	if b.conn != nil {
		_ = b.conn.Quit()
		b.conn = nil
	}
	b.abortConns()
}

func (b *Backend) toServer(p string) string {
	if b.enc == nil {
		return p
	}
	s, err := b.enc.NewEncoder().String(p)
	if err != nil {
		return p
	}
	return s
}

func (b *Backend) fromServer(name string) string {
	if b.enc == nil {
		return name
	}
	s, err := b.enc.NewDecoder().String(name)
	if err != nil {
		return name
	}
	return s
}
