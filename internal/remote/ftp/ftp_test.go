package ftp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/spf13/afero"
	"golang.org/x/text/encoding/charmap"

	"dsfetch/internal/remote"
	"dsfetch/internal/remote/ftp/ftptest"
)

func startServer(t *testing.T, s *ftptest.Server) remote.Config {
	t.Helper()
	return s.Start(t)
}

func connect(t *testing.T, cfg remote.Config) *Backend {
	t.Helper()
	b := New(cfg)
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func names(entries []remote.Entry) []string {
	var out []string
	for _, e := range entries {
		s := e.Name
		if e.IsDir {
			s += "/"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestListCyrillicUTF8(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/Игры/NDS", 0o755)
	_ = afero.WriteFile(fs, "/Игры/NDS/Покемон.nds", []byte("rom"), 0o644)
	_ = afero.WriteFile(fs, "/readme.txt", []byte("hello"), 0o644)
	cfg := startServer(t, &ftptest.Server{FS: fs})
	b := connect(t, cfg)

	root, err := b.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(root); len(got) != 2 || got[0] != "readme.txt" || got[1] != "Игры/" {
		t.Fatalf("root = %v", got)
	}
	sub, err := b.List(context.Background(), "/Игры/NDS")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "Покемон.nds" || sub[0].Size != 3 || sub[0].IsDir {
		t.Fatalf("sub = %+v", sub)
	}
}

func TestLegacyCP1251Server(t *testing.T) {
	// The server stores and sends raw CP1251 bytes, like an old Windows FTP.
	enc := charmap.Windows1251.NewEncoder()
	dir, _ := enc.String("Игры")
	file, _ := enc.String("Тетрис.gb")
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/"+dir, 0o755)
	_ = afero.WriteFile(fs, "/"+dir+"/"+file, []byte("tetris"), 0o644)
	cfg := startServer(t, &ftptest.Server{FS: fs})
	cfg.Encoding = "cp1251"
	b := connect(t, cfg)

	root, err := b.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root[0].Name != "Игры" || !root[0].IsDir {
		t.Fatalf("root = %+v", root)
	}
	sub, err := b.List(context.Background(), "/Игры")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "Тетрис.gb" {
		t.Fatalf("sub = %+v", sub)
	}
	r, err := b.Open(context.Background(), "/Игры/Тетрис.gb", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != "tetris" {
		t.Fatalf("data = %q", data)
	}
}

func TestOpenWithOffsetResumes(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 1000)
	fs := afero.NewMemMapFs()
	_ = afero.WriteFile(fs, "/game.bin", payload, 0o644)
	b := connect(t, startServer(t, &ftptest.Server{FS: fs}))

	r, err := b.Open(context.Background(), "/game.bin", 4321)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[4321:]) {
		t.Fatalf("resumed data mismatch: got %d bytes", len(got))
	}
	// The control connection is usable again after the transfer.
	if _, err := b.List(context.Background(), "/"); err != nil {
		t.Fatalf("list after transfer: %v", err)
	}
}

func TestWrongPassword(t *testing.T) {
	cfg := startServer(t, &ftptest.Server{})
	cfg.Password = "wrong"
	b := New(cfg)
	err := b.Connect(context.Background())
	if !errors.Is(err, remote.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestAnonymous(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = afero.WriteFile(fs, "/pub.txt", []byte("x"), 0o644)
	cfg := startServer(t, &ftptest.Server{FS: fs, AllowAnon: true})
	cfg.User, cfg.Password, cfg.Anonymous = "", "", true
	b := connect(t, cfg)
	if _, err := b.List(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}

	cfg2 := startServer(t, &ftptest.Server{FS: fs})
	cfg2.User, cfg2.Password, cfg2.Anonymous = "", "", true
	if err := New(cfg2).Connect(context.Background()); !errors.Is(err, remote.ErrAuth) {
		t.Fatalf("anonymous on closed server: %v", err)
	}
}

func TestConnectRefusedIsNotAuthError(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	b := New(remote.Config{Host: "127.0.0.1", Port: port, DialTimeout: 2 * time.Second})
	err := b.Connect(context.Background())
	if err == nil || errors.Is(err, remote.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelUnblocksStalledDownload(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = afero.WriteFile(fs, "/big.bin", bytes.Repeat([]byte{1}, 100_000), 0o644)
	b := connect(t, startServer(t, &ftptest.Server{FS: fs, Faults: ftptest.Faults{StallAfter: 1000}}))

	ctx, cancel := context.WithCancel(context.Background())
	r, err := b.Open(ctx, "/big.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read still blocked after cancel")
	}
	_ = r.Close()
}

func TestFTPSFingerprintPinning(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = afero.WriteFile(fs, "/secure.txt", []byte("tls data"), 0o644)
	cfg := startServer(t, &ftptest.Server{FS: fs, TLS: selfSignedTLS(t)})
	cfg.TLS = true

	// First use: accepted, fingerprint reported for pinning.
	b := connect(t, cfg)
	fp := b.Fingerprint()
	if fp == "" {
		t.Fatal("no fingerprint after TLS connect")
	}
	r, err := b.Open(context.Background(), "/secure.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != "tls data" {
		t.Fatalf("data over TLS = %q", data)
	}

	// Pinned and matching: fine.
	cfg.KnownFingerprint = fp
	connect(t, cfg)

	// Pinned but different: refused with a mismatch error.
	cfg.KnownFingerprint = "SHA256:somethingelse"
	err = New(cfg).Connect(context.Background())
	var mismatch *remote.HostKeyMismatchError
	if !errors.As(err, &mismatch) || mismatch.Got != fp {
		t.Fatalf("err = %v, want HostKeyMismatchError", err)
	}
}

// vsftpd and ProFTPD require the data connections to resume the control
// connection's TLS session.
func TestFTPSSessionReuse(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/games", 0o755)
	_ = afero.WriteFile(fs, "/games/rom.nds", []byte("tls data"), 0o644)
	cfg := startServer(t, &ftptest.Server{FS: fs, TLS: selfSignedTLS(t), RequireSessionReuse: true})
	cfg.TLS = true

	b := connect(t, cfg)
	entries, err := b.List(context.Background(), "/games")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "rom.nds" {
		t.Fatalf("entries = %+v", entries)
	}
	r, err := b.Open(context.Background(), "/games/rom.nds", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	if err := r.Close(); err != nil || string(data) != "tls data" {
		t.Fatalf("download = %q, close err %v", data, err)
	}
}

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nas.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}
