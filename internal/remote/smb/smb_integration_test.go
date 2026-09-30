//go:build integration

// Integration tests against a real Samba server:
//
//	task test:integration
//
// Defaults match dev/docker-compose.yml; override with SMB_TEST_ADDR,
// SMB_TEST_USER, SMB_TEST_PASS, SMB_TEST_SHARE.
package smb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	smb2 "github.com/cloudsoda/go-smb2"

	"dsfetch/internal/remote"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func testConfig(t *testing.T) (remote.Config, string) {
	t.Helper()
	addr := env("SMB_TEST_ADDR", "127.0.0.1:4445")
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("no SMB server at %s (run `task dev:servers`): %v", addr, err)
	}
	conn.Close()
	return remote.Config{
		Protocol: "smb", Host: host, Port: port,
		User: env("SMB_TEST_USER", "test"), Password: env("SMB_TEST_PASS", "test"),
		Keepalive: -1,
	}, env("SMB_TEST_SHARE", "Data")
}

// seed writes test files with go-smb2 directly (our backend is read-only).
func seed(t *testing.T, cfg remote.Config, share string, files map[string][]byte) {
	t.Helper()
	os.RemoveAll(filepath.Join(env("SMB_TEST_SHARE_DIR", "../../../dev/share"), "dsfetch-it"))
	conn, err := net.Dial("tcp", cfg.Address(445))
	if err != nil {
		t.Fatal(err)
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: cfg.User, Password: cfg.Password}}
	s, err := d.DialConn(context.Background(), conn, cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Logoff()
	fs, err := s.Mount(share)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Umount()
	for name, data := range files {
		if err := fs.MkdirAll(dirOf(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		// Docker Desktop maps bind-mounted files to root, so deleting over
		// SMB can be refused; the share is dev/share on this machine too.
		defer os.RemoveAll(filepath.Join(env("SMB_TEST_SHARE_DIR", "../../../dev/share"), "dsfetch-it"))
		conn, err := net.Dial("tcp", cfg.Address(445))
		if err != nil {
			return
		}
		s, err := d.DialConn(context.Background(), conn, cfg.Host)
		if err != nil {
			return
		}
		defer s.Logoff()
		if fs, err := s.Mount(share); err == nil {
			_ = fs.RemoveAll("dsfetch-it")
			fs.Umount()
		}
	})
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func TestSharesListingAndDownload(t *testing.T) {
	cfg, share := testConfig(t)
	payload := bytes.Repeat([]byte("smb!"), 300_000) // 1.2 MB
	seed(t, cfg, share, map[string][]byte{
		"dsfetch-it/Игры/Тест.nds": payload,
		"dsfetch-it/readme.txt":    []byte("hi"),
	})

	b := New(cfg)
	ctx := context.Background()
	if err := b.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	shares, err := b.List(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range shares {
		if s.Name == "IPC$" {
			t.Fatal("IPC$ must be hidden")
		}
		if s.Name == share && s.IsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("share %q not listed: %+v", share, shares)
	}

	entries, err := b.List(ctx, "/"+share+"/dsfetch-it/Игры")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "Тест.nds" || entries[0].Size != int64(len(payload)) {
		t.Fatalf("entries = %+v", entries)
	}

	r, err := b.Open(ctx, "/"+share+"/dsfetch-it/Игры/Тест.nds", 777_777)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[777_777:]) {
		t.Fatalf("resumed read: %d bytes", len(got))
	}
}

func TestWrongPassword(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.Password = "definitely-wrong"
	err := New(cfg).Connect(context.Background())
	if !errors.Is(err, remote.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}
