package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServersRoundTripAndEncryption(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	srv := Server{Name: "NAS", Protocol: ProtoSFTP, Host: "192.168.1.10", Username: "pi", SavePassword: true}
	const secret = "s3cr3t-пароль"
	if err := st.SetPassword(&srv, secret); err != nil {
		t.Fatal(err)
	}
	saved, err := st.Put(srv)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("expected an ID to be assigned")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("password stored in clear text:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"password_enc": "v2:`) {
		t.Fatalf("expected encrypted password field:\n%s", raw)
	}

	// Reopen: same key file, so the password decrypts.
	st2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.Get(saved.ID)
	if !ok {
		t.Fatal("server missing after reopen")
	}
	if p, ok := st2.Password(got); !ok || p != secret {
		t.Fatalf("password = %q, %v", p, ok)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(Server{Name: "x", Protocol: ProtoFTP, Host: "h"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"servers.json", ".key"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
}

func TestPasswordNotSavedWhenDisabled(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := Server{Protocol: ProtoFTP, Host: "h", PasswordEnc: "v1:old", SavePassword: false}
	if err := st.SetPassword(&srv, "pw"); err != nil {
		t.Fatal(err)
	}
	if srv.PasswordEnc != "" {
		t.Fatalf("PasswordEnc = %q, want empty", srv.PasswordEnc)
	}
}

func TestLostKeyMeansAskAgain(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, nil)
	srv := Server{Protocol: ProtoFTP, Host: "h", SavePassword: true}
	_ = st.SetPassword(&srv, "pw")
	srv, _ = st.Put(srv)

	if err := os.Remove(filepath.Join(dir, ".key")); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st2.Get(srv.ID)
	if _, ok := st2.Password(got); ok {
		t.Fatal("expected undecryptable password with a new key")
	}
}

// A damaged servers.json must not stop DSFetch: it is kept as .bad and the
// list starts empty.
func TestDamagedServersFileIsSetAside(t *testing.T) {
	dir := t.TempDir()
	damaged := []byte(`{"servers": [ {"id": "x", "name": "NAS"`)
	os.WriteFile(filepath.Join(dir, "servers.json"), damaged, 0o600)

	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.SetAside(); len(got) != 1 || got[0] != "servers.json" {
		t.Fatalf("SetAside = %v", got)
	}
	if n := len(st.Servers()); n != 0 {
		t.Fatalf("%d servers, want none", n)
	}
	if kept, _ := os.ReadFile(filepath.Join(dir, "servers.json"+BadSuffix)); !bytes.Equal(kept, damaged) {
		t.Fatalf(".bad holds %q", kept)
	}
	if _, err := st.Put(Server{Protocol: ProtoFTP, Host: "h"}); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir, nil)
	if err != nil || len(st2.Servers()) != 1 || len(st2.SetAside()) != 0 {
		t.Fatalf("after saving: %d servers, set aside %v, %v", len(st2.Servers()), st2.SetAside(), err)
	}
}

// A damaged key is replaced; passwords saved with it are asked again.
func TestDamagedKeyIsSetAside(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, nil)
	srv := Server{Protocol: ProtoFTP, Host: "h", SavePassword: true}
	_ = st.SetPassword(&srv, "pw")
	srv, _ = st.Put(srv)
	os.WriteFile(filepath.Join(dir, ".key"), []byte("short"), 0o600)

	st2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.SetAside(); len(got) != 1 || got[0] != ".key" {
		t.Fatalf("SetAside = %v", got)
	}
	if key, _ := os.ReadFile(filepath.Join(dir, ".key")); len(key) != 32 {
		t.Fatalf("new key has %d bytes", len(key))
	}
	got, _ := st2.Get(srv.ID)
	if _, ok := st2.Password(got); ok {
		t.Fatal("expected an undecryptable password with the new key")
	}
}

func TestUpdateDeleteAndSort(t *testing.T) {
	st, _ := Open(t.TempDir(), nil)
	b, _ := st.Put(Server{Name: "beta", Protocol: ProtoSMB, Host: "b"})
	a, _ := st.Put(Server{Name: "Alpha", Protocol: ProtoFTP, Host: "a"})

	list := st.Servers()
	if len(list) != 2 || list[0].ID != a.ID {
		t.Fatalf("unexpected order: %+v", list)
	}

	err := st.Update(b.ID, func(s *Server) {
		if s.LastTargets == nil {
			s.LastTargets = map[string]string{}
		}
		s.LastTargets["/roms/nds"] = "/mnt/mmc/Roms/NDS"
		s.HostKey = "SHA256:abc"
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(b.ID)
	if got.LastTargets["/roms/nds"] != "/mnt/mmc/Roms/NDS" || got.HostKey != "SHA256:abc" {
		t.Fatalf("update lost: %+v", got)
	}

	if err := st.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Servers()); n != 1 {
		t.Fatalf("len = %d after delete", n)
	}
}

func TestSecretBoxRejectsTampering(t *testing.T) {
	box, _ := NewAESBox(make([]byte, 32), nil)
	ctx := []byte("ftp\x00host\x0021\x00user")
	enc, err := box.Seal("hello", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open(enc, ctx); err != nil || got != "hello" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	tampered := enc[:len(enc)-2] + "AA"
	if _, err := box.Open(tampered, ctx); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := box.Open(enc, []byte("ftp\x00other\x0021\x00user")); err == nil {
		t.Fatal("secret opened with another context")
	}
	if _, err := box.Open("plain", ctx); err == nil {
		t.Fatal("unprefixed value accepted")
	}
}

// sealV1 writes a password the way versions before v2 did: the key file
// directly, no context.
func sealV1(t *testing.T, key []byte, plain string) string {
	t.Helper()
	aead, err := newGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	return "v1:" + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil))
}

func TestLegacyPasswordsAreSealedAgain(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	os.WriteFile(filepath.Join(dir, ".key"), key, 0o600)
	srv := Server{ID: "a1", Protocol: ProtoSMB, Host: "nas", Username: "me", SavePassword: true,
		PasswordEnc: sealV1(t, key, "old-secret")}
	data, _ := json.Marshal(serversFile{Version: 1, Servers: []Server{srv}})
	os.WriteFile(filepath.Join(dir, "servers.json"), data, 0o600)

	st, err := Open(dir, []byte("console-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("a1")
	if !strings.HasPrefix(got.PasswordEnc, "v2:") {
		t.Fatalf("not sealed again: %q", got.PasswordEnc)
	}
	if p, ok := st.Password(got); !ok || p != "old-secret" {
		t.Fatalf("password = %q, %v", p, ok)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "servers.json"))
	if strings.Contains(string(raw), `"v1:`) {
		t.Fatalf("servers.json still holds a v1 password:\n%s", raw)
	}
}

// A card read in another console (or a computer) does not give the
// passwords away: they are asked for again.
func TestPasswordsAreBoundToTheConsole(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, []byte("console-1"))
	srv := Server{Protocol: ProtoSFTP, Host: "nas", Username: "me", SavePassword: true}
	_ = st.SetPassword(&srv, "secret")
	srv, _ = st.Put(srv)

	for id, want := range map[string]bool{"console-1": true, "console-2": false, "": false} {
		var devID []byte
		if id != "" {
			devID = []byte(id)
		}
		st2, err := Open(dir, devID)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := st2.Get(srv.ID)
		if p, ok := st2.Password(got); ok != want || (ok && p != "secret") {
			t.Errorf("device %q: %q, %v; want ok=%v", id, p, ok, want)
		}
	}
}

// A saved password only opens for the server it was saved for: pointing the
// entry elsewhere without the editor makes DSFetch ask for it.
func TestPasswordsAreBoundToTheServer(t *testing.T) {
	st, _ := Open(t.TempDir(), nil)
	srv := Server{Protocol: ProtoFTP, Host: "nas", Username: "me", SavePassword: true}
	_ = st.SetPassword(&srv, "secret")
	srv, _ = st.Put(srv)

	_ = st.Update(srv.ID, func(s *Server) { s.Host = "evil.example" })
	moved, _ := st.Get(srv.ID)
	if _, ok := st.Password(moved); ok {
		t.Fatal("password opened for another host")
	}
	// The editor seals it again for the new address.
	_ = st.SetPassword(&moved, "secret")
	if p, ok := st.Password(moved); !ok || p != "secret" {
		t.Fatalf("after sealing again: %q, %v", p, ok)
	}
	// Name and folders are not part of the binding.
	moved.Name, moved.InitialPath = "NAS", "/games"
	if _, ok := st.Password(moved); !ok {
		t.Fatal("renaming broke the password")
	}
}

func TestSettingsDefaultsAndSave(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s != DefaultSettings() {
		t.Fatalf("defaults: %+v", s)
	}
	s.Language = "ru"
	s.DeleteArchive = Always
	if err := SaveSettings(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Fatalf("got %+v want %+v", got, s)
	}
}
