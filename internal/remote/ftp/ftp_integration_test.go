//go:build integration

// Integration tests against the vsftpd container from dev/docker-compose.yml
// (task test:integration). Override with FTP_TEST_ADDR, FTP_TEST_DIR (the
// share path on the server) and FTP_TEST_SHARE (the same folder on this
// machine).
package ftp

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dsfetch/internal/remote"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestVsftpdSymlinks(t *testing.T) {
	addr := env("FTP_TEST_ADDR", "127.0.0.1:2121")
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		t.Skipf("no FTP server at %s (run `task dev:servers`): %v", addr, err)
	} else {
		c.Close()
	}
	share := env("FTP_TEST_SHARE", "../../../dev/share")
	serverDir := env("FTP_TEST_DIR", "/ftp/test")

	dir := filepath.Join(share, "dsfetch-links")
	os.RemoveAll(dir)
	os.MkdirAll(filepath.Join(dir, "real-dir"), 0o755)
	os.WriteFile(filepath.Join(dir, "real-file.nds"), []byte("rom"), 0o644)
	// Relative targets so they resolve inside the container as well.
	os.Symlink("real-dir", filepath.Join(dir, "dir-link"))
	os.Symlink("real-file.nds", filepath.Join(dir, "file-link.nds"))
	t.Cleanup(func() { os.RemoveAll(dir) })

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	b := New(remote.Config{Host: host, Port: port, User: "test", Password: "test", Keepalive: -1})
	if err := b.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	entries, err := b.List(context.Background(), serverDir+"/dsfetch-links")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]remote.Entry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if e := got["dir-link"]; !e.IsDir || e.IsLink {
		t.Errorf("dir-link = %+v, want a folder", e)
	}
	if e := got["file-link.nds"]; e.IsDir {
		t.Errorf("file-link.nds = %+v, want a file", e)
	}
	if _, err := b.List(context.Background(), serverDir+"/dsfetch-links/dir-link"); err != nil {
		t.Fatalf("list through link: %v", err)
	}
}

// vsftpd with TLS refuses data connections that do not resume the control
// connection's TLS session ("522 SSL connection failed: session reuse
// required"): listing and downloading must work anyway.
func TestVsftpdFTPS(t *testing.T) {
	addr := env("FTPS_TEST_ADDR", "127.0.0.1:2122")
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		t.Skipf("no FTPS server at %s (run `task dev:servers`): %v", addr, err)
	} else {
		c.Close()
	}
	share := env("FTP_TEST_SHARE", "../../../dev/share")
	serverDir := env("FTP_TEST_DIR", "/ftp/test")

	dir := filepath.Join(share, "dsfetch-ftps")
	os.RemoveAll(dir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "rom.nds"), []byte("over tls"), 0o644)
	t.Cleanup(func() { os.RemoveAll(dir) })

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	b := New(remote.Config{Host: host, Port: port, User: "test", Password: "test", TLS: true, Keepalive: -1})
	if err := b.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Fingerprint() == "" {
		t.Error("no certificate fingerprint to pin")
	}

	entries, err := b.List(context.Background(), serverDir+"/dsfetch-ftps")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "rom.nds" {
		t.Fatalf("entries = %+v", entries)
	}
	r, err := b.Open(context.Background(), serverDir+"/dsfetch-ftps/rom.nds", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	if err := r.Close(); err != nil || string(data) != "over tls" {
		t.Fatalf("download = %q, close err %v", data, err)
	}
}
