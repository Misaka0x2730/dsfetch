package sftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"dsfetch/internal/remote"
	"dsfetch/internal/remote/sftp/sftptest"
)

func connect(t *testing.T, cfg remote.Config) *Backend {
	t.Helper()
	b := New(cfg)
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestListCyrillicAndSymlinks(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Игры", "NDS"), 0o755)
	os.WriteFile(filepath.Join(root, "Игры", "NDS", "Покемон.nds"), []byte("rom!"), 0o644)
	os.Symlink(filepath.Join(root, "Игры"), filepath.Join(root, "games-link"))
	os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling"))

	b := connect(t, (&sftptest.Server{Root: root}).Start(t))
	entries, err := b.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name+":"+strconv.FormatBool(e.IsDir))
	}
	sort.Strings(got)
	want := []string{"games-link:true", "Игры:true"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
	sub, err := b.List(context.Background(), "/Игры/NDS")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "Покемон.nds" || sub[0].Size != 4 {
		t.Fatalf("sub = %+v", sub)
	}
}

func TestOpenOffset(t *testing.T) {
	root := t.TempDir()
	payload := bytes.Repeat([]byte("abcdefghij"), 50_000) // 500 KB, several SFTP packets
	os.WriteFile(filepath.Join(root, "iso.bin"), payload, 0o644)
	b := connect(t, (&sftptest.Server{Root: root}).Start(t))

	for _, off := range []int64{0, 1, 123_457} {
		r, err := b.Open(context.Background(), "/iso.bin", off)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil { // exercises WriteTo
			t.Fatal(err)
		}
		r.Close()
		if !bytes.Equal(buf.Bytes(), payload[off:]) {
			t.Fatalf("offset %d: got %d bytes", off, buf.Len())
		}
	}
}

func TestWrongPassword(t *testing.T) {
	cfg := (&sftptest.Server{Root: t.TempDir()}).Start(t)
	cfg.Password = "nope"
	if err := New(cfg).Connect(context.Background()); !errors.Is(err, remote.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

// pin is how a host key is saved: its type and SHA256 fingerprint.
func pin(k ssh.Signer) string {
	return k.PublicKey().Type() + " " + remote.Fingerprint(k.PublicKey().Marshal())
}

func TestHostKeyTOFU(t *testing.T) {
	srv := &sftptest.Server{Root: t.TempDir()}
	cfg := srv.Start(t)

	b := connect(t, cfg)
	fp := b.Fingerprint()
	if fp != pin(srv.HostKey) {
		t.Fatalf("fingerprint = %q, want %q", fp, pin(srv.HostKey))
	}

	cfg.KnownFingerprint = fp
	connect(t, cfg)

	// Same address, new host key (reinstalled server or MITM).
	other := &sftptest.Server{Root: srv.Root}
	cfg2 := other.Start(t)
	cfg2.KnownFingerprint = fp
	err := New(cfg2).Connect(context.Background())
	var mismatch *remote.HostKeyMismatchError
	if !errors.As(err, &mismatch) || mismatch.Known != fp {
		t.Fatalf("err = %v, want HostKeyMismatchError", err)
	}
}

// A server with several keys must keep presenting the pinned one, whatever
// order x/crypto prefers (it picks ECDSA before Ed25519 today).
func TestHostKeyPinnedTypeIsAskedFor(t *testing.T) {
	ed := sftptest.NewHostKey(t)
	ec := sftptest.NewECDSAHostKey(t)
	srv := &sftptest.Server{Root: t.TempDir(), HostKey: ed, MoreHostKeys: []ssh.Signer{ec}}
	cfg := srv.Start(t)

	if fp := connect(t, cfg).Fingerprint(); fp != pin(ec) {
		t.Fatalf("first use pinned %q, want the ECDSA key %q", fp, pin(ec))
	}
	cfg.KnownFingerprint = pin(ed)
	if fp := connect(t, cfg).Fingerprint(); fp != pin(ed) {
		t.Fatalf("pinned Ed25519, got %q", fp)
	}
}

// Pins saved before the key type was stored still match, and the backend
// reports the full form for saving.
func TestHostKeyPinWithoutType(t *testing.T) {
	srv := &sftptest.Server{Root: t.TempDir()}
	cfg := srv.Start(t)
	cfg.KnownFingerprint = remote.Fingerprint(srv.HostKey.PublicKey().Marshal())
	if fp := connect(t, cfg).Fingerprint(); fp != pin(srv.HostKey) {
		t.Fatalf("fingerprint = %q, want %q", fp, pin(srv.HostKey))
	}
}

// When the server has no key of the pinned type any more, the user sees a
// changed key (and can accept it), not a failed negotiation.
func TestHostKeyTypeGone(t *testing.T) {
	srv := &sftptest.Server{Root: t.TempDir()}
	cfg := srv.Start(t)
	cfg.KnownFingerprint = "ssh-rsa SHA256:formerRSAkey"
	err := New(cfg).Connect(context.Background())
	var mismatch *remote.HostKeyMismatchError
	if !errors.As(err, &mismatch) || mismatch.Got != pin(srv.HostKey) {
		t.Fatalf("err = %v, want HostKeyMismatchError offering %q", err, pin(srv.HostKey))
	}
}

func TestCancelUnblocksStalledRead(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 200_000), 0o644)
	b := connect(t, (&sftptest.Server{Root: root, Faults: sftptest.Faults{StallAfter: 40_000}}).Start(t))

	ctx, cancel := context.WithCancel(context.Background())
	r, err := b.Open(ctx, "/big.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, r)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read still blocked after cancel")
	}
	r.Close()
}
