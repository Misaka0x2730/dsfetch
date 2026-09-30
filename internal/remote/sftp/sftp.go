// Package sftp implements remote.Backend over SFTP (x/crypto/ssh + pkg/sftp).
//
// The host key is pinned trust-on-first-use: the first connection reports
// its type and SHA256 fingerprint for saving ("ssh-ed25519 SHA256:..."),
// later connections ask for a key of that type and must match it. Login is
// by password (also answered for keyboard-interactive prompts).
package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"sync"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"dsfetch/internal/remote"
)

// Backend is one SSH connection with an SFTP session.
type Backend struct {
	cfg remote.Config

	mu     sync.Mutex
	raw    net.Conn
	ssh    *ssh.Client
	client *psftp.Client

	fingerprint string
	stop        chan struct{}
}

// New returns an unconnected backend.
func New(cfg remote.Config) *Backend {
	return &Backend{cfg: cfg}
}

// Fingerprint of the server host key, valid after Connect.
func (b *Backend) Fingerprint() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fingerprint
}

// Connect dials, authenticates and opens the SFTP subsystem.
func (b *Backend) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return nil
	}
	pinnedType, _ := splitPin(b.cfg.KnownFingerprint)
	algos := hostKeyAlgorithms(pinnedType)
	err := b.dial(ctx, algos)
	var neg *ssh.AlgorithmNegotiationError
	if algos != nil && errors.As(err, &neg) && neg.What == "host key" {
		// No key of the pinned type any more: take any, so that the
		// change shows as a key mismatch the user can accept.
		err = b.dial(ctx, nil)
	}
	return err
}

// splitPin splits a pinned key "ssh-ed25519 SHA256:..." into its type and
// fingerprint. Pins saved by older versions have no type.
func splitPin(pin string) (keyType, fp string) {
	if t, f, ok := strings.Cut(pin, " "); ok {
		return t, f
	}
	return "", pin
}

// hostKeyAlgorithms returns the host key algorithms for a pinned key type
// (nil: any). A server usually has several keys, and without this the
// client's preference order picks one, which a newer x/crypto may change.
func hostKeyAlgorithms(keyType string) []string {
	switch keyType {
	case "":
		return nil
	case ssh.KeyAlgoRSA: // the key type; these are its signature algorithms
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	default:
		return []string{keyType}
	}
}

// dial connects with the given host key algorithms (nil: the defaults).
func (b *Backend) dial(ctx context.Context, hostKeyAlgos []string) error {
	timeout := b.cfg.DialTimeoutOrDefault()
	addr := b.cfg.Address(22)

	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("sftp: connect: %w", err)
	}
	// Bound the SSH handshake and login; cancel also aborts it.
	_ = raw.SetDeadline(time.Now().Add(2 * timeout))
	stopAbort := remote.AbortOnCancel(ctx, func() { raw.Close() })
	defer stopAbort()

	password := b.cfg.Password
	conf := &ssh.ClientConfig{
		User: b.cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := remote.Fingerprint(key.Marshal())
			b.fingerprint = key.Type() + " " + fp
			knownType, knownFP := splitPin(b.cfg.KnownFingerprint)
			if knownFP == "" || knownFP == fp && (knownType == "" || knownType == key.Type()) {
				return nil
			}
			return &remote.HostKeyMismatchError{Known: b.cfg.KnownFingerprint, Got: b.fingerprint}
		},
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           timeout,
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(raw, addr, conf)
	if err != nil {
		raw.Close()
		return mapHandshakeError(ctx, err)
	}
	_ = raw.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(sshConn, chans, reqs)

	client, err := psftp.NewClient(sshClient,
		psftp.UseConcurrentReads(true),
		psftp.MaxConcurrentRequestsPerFile(64),
	)
	if err != nil {
		sshClient.Close()
		return fmt.Errorf("sftp: start subsystem: %w", err)
	}
	b.raw, b.ssh, b.client = raw, sshClient, client
	b.startKeepalive()
	return nil
}

func mapHandshakeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var mismatch *remote.HostKeyMismatchError
	if errors.As(err, &mismatch) {
		return mismatch
	}
	if strings.Contains(err.Error(), "unable to authenticate") {
		return fmt.Errorf("%w: %v", remote.ErrAuth, err)
	}
	return fmt.Errorf("sftp: handshake: %w", err)
}

func (b *Backend) startKeepalive() {
	interval := b.cfg.KeepaliveOrDefault()
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	b.stop = stop
	sshClient, raw := b.ssh, b.raw
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				reply := make(chan error, 1)
				go func() {
					_, _, err := sshClient.SendRequest("keepalive@openssh.com", true, nil)
					reply <- err
				}()
				select {
				case <-reply:
				case <-time.After(15 * time.Second):
					// No answer: the link is dead. Closing makes the next
					// operation fail fast instead of hanging.
					raw.Close()
					return
				case <-stop:
					return
				}
			}
		}
	}()
}

func (b *Backend) current() (*psftp.Client, net.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return nil, nil, remote.ErrNotConnected
	}
	return b.client, b.raw, nil
}

// List returns directory entries; symlinks are resolved to see if they
// point at a directory.
func (b *Backend) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	client, raw, err := b.current()
	if err != nil {
		return nil, err
	}
	stop := remote.AbortOnCancel(ctx, func() { raw.Close() })
	defer stop()

	dir = remote.Clean(dir)
	infos, err := client.ReadDir(dir)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("sftp: list %s: %w", dir, err)
	}
	out := make([]remote.Entry, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		if name == "." || name == ".." {
			continue
		}
		e := remote.Entry{Name: name, Size: fi.Size(), IsDir: fi.IsDir(), ModTime: fi.ModTime()}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if target, err := client.Stat(remote.Join(dir, name)); err == nil {
				e.IsDir = target.IsDir()
				e.Size = target.Size()
			} else {
				continue // dangling link
			}
		}
		out = append(out, e)
	}
	return out, nil
}

type fileReader struct {
	*psftp.File // keeps WriteTo: pkg/sftp reads ahead concurrently
	stop        func() bool
}

func (r *fileReader) Close() error {
	r.stop()
	return r.File.Close()
}

// Open returns the file positioned at offset.
func (b *Backend) Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	client, raw, err := b.current()
	if err != nil {
		return nil, err
	}
	stop := remote.AbortOnCancel(ctx, func() { raw.Close() })
	f, err := client.Open(remote.Clean(path))
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("sftp: open %s: %w", path, err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			stop()
			f.Close()
			return nil, fmt.Errorf("sftp: seek: %w", err)
		}
	}
	return &fileReader{File: f, stop: stop}, nil
}

// Close ends the session.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
	if b.client != nil {
		_ = b.client.Close()
		b.client = nil
	}
	if b.ssh != nil {
		_ = b.ssh.Close()
		b.ssh = nil
	}
	if b.raw != nil {
		_ = b.raw.Close()
		b.raw = nil
	}
	return nil
}
