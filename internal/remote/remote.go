// Package remote defines the protocol-independent view of a file server.
// Implementations live in the ftp, sftp and smb subpackages; backends.New
// picks one by protocol.
//
// Paths are always slash-separated and absolute ("/games/nds"). For SMB the
// first path element is the share name and "/" lists the shares.
package remote

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Entry is one item of a directory listing.
type Entry struct {
	Name    string
	Size    int64
	IsDir   bool
	IsLink  bool // symlink whose target type is unknown (FTP); try List, else download
	ModTime time.Time
}

// Backend is a single connection to a server. It is not safe for concurrent
// use: browsing and downloading use separate backends (FTP cannot list while
// a RETR is running on the same connection).
type Backend interface {
	Connect(ctx context.Context) error
	List(ctx context.Context, dir string) ([]Entry, error)
	// Open streams a file starting at offset (offset > 0 resumes a download).
	// While the reader is open no other call may be made on the backend.
	Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
	Close() error
}

// Fingerprinter is implemented by backends that pin a server identity
// (SFTP host key, FTPS certificate). Fingerprint is valid after Connect.
type Fingerprinter interface {
	Fingerprint() string
}

// Config describes how to reach and log in to a server.
type Config struct {
	Protocol  string // "ftp", "sftp" or "smb"
	Host      string
	Port      int
	User      string
	Password  string
	Anonymous bool   // FTP anonymous / SMB guest
	Domain    string // SMB, optional

	TLS      bool   // FTP: explicit TLS (AUTH TLS)
	Encoding string // FTP: "utf-8" (default), "cp1251", "cp866"

	// KnownFingerprint is the pinned SHA256 fingerprint. Empty means trust
	// on first use: Connect succeeds and Fingerprint() reports the key to
	// save. A different key fails with *HostKeyMismatchError.
	KnownFingerprint string

	DialTimeout time.Duration // default 10 s
	Keepalive   time.Duration // default 30 s; negative disables
}

// Address returns host:port.
func (c Config) Address(defaultPort int) string {
	port := c.Port
	if port <= 0 {
		port = defaultPort
	}
	host := c.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6 literal
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// DialTimeoutOrDefault returns the dial timeout to use.
func (c Config) DialTimeoutOrDefault() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 10 * time.Second
}

// KeepaliveOrDefault returns the keepalive interval (0 = disabled).
func (c Config) KeepaliveOrDefault() time.Duration {
	switch {
	case c.Keepalive < 0:
		return 0
	case c.Keepalive == 0:
		return 30 * time.Second
	default:
		return c.Keepalive
	}
}

var (
	// ErrAuth means the server rejected the login or password.
	ErrAuth = errors.New("remote: authentication failed")
	// ErrNotConnected is returned when Connect has not succeeded.
	ErrNotConnected = errors.New("remote: not connected")
	// ErrResumeUnsupported means the server refused to start at an offset;
	// the download has to restart from zero.
	ErrResumeUnsupported = errors.New("remote: server does not support resume")
)

// HostKeyMismatchError is returned when the server identity differs from the
// pinned fingerprint (possible man-in-the-middle, or the server was
// reinstalled).
type HostKeyMismatchError struct {
	Known string
	Got   string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("remote: server key changed (pinned %s, got %s)", e.Known, e.Got)
}

// Fingerprint formats a SHA256 fingerprint like OpenSSH does.
func Fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// CheckFingerprint implements the trust-on-first-use rule.
func CheckFingerprint(known, got string) error {
	if known == "" || known == got {
		return nil
	}
	return &HostKeyMismatchError{Known: known, Got: got}
}

// Clean normalises a remote path to an absolute slash path.
func Clean(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean("/" + p)
}

// Join joins a directory and a name.
func Join(dir, name string) string {
	return path.Join(Clean(dir), name)
}

// Parent returns the parent directory ("/" stays "/").
func Parent(p string) string {
	return path.Dir(Clean(p))
}

// SortEntries puts directories first, then sorts by name case-insensitively.
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// AbortOnCancel runs abort when ctx is cancelled while an operation is in
// flight. Closing the connection is the only reliable way to unblock a read
// stuck on a dead Wi-Fi link. Call the returned stop when done.
func AbortOnCancel(ctx context.Context, abort func()) (stop func() bool) {
	return context.AfterFunc(ctx, abort)
}

// ReadCloserFunc adapts a reader plus a close function.
type ReadCloserFunc struct {
	io.Reader
	CloseFunc func() error
}

func (r ReadCloserFunc) Close() error { return r.CloseFunc() }
