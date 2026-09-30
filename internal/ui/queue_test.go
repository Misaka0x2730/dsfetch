package ui

import (
	"os"
	"path/filepath"
	"testing"

	"dsfetch/internal/store"
	"dsfetch/internal/transfer"
)

// A multi-volume archive is unpacked once, after its last volume in the
// batch, from its first volume; single archives right away.
func TestExtractableAfter(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"a.zip",
		"b.part1.rar", "b.part2.rar",
		"c.rar", "c.r00", "c.r01",
		"d.7z.001", "d.7z.002",
		"readme.txt",
	}
	items := make([]transfer.Item, len(names))
	for i, n := range names {
		items[i].LocalPath = filepath.Join(dir, n)
		if err := os.WriteFile(items[i].LocalPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"a.zip": "a.zip", "b.part2.rar": "b.part1.rar", "c.r01": "c.rar", "d.7z.002": "d.7z.001"}
	for i, n := range names {
		got, ok := extractableAfter(items, i)
		w, should := want[n]
		if ok != should || (ok && got != filepath.Join(dir, w)) {
			t.Errorf("after %s: %q, %v; want %q, %v", n, got, ok, w, should)
		}
	}
}

func TestPlainFTPLogin(t *testing.T) {
	for _, c := range []struct {
		srv  store.Server
		want bool
	}{
		{store.Server{Protocol: store.ProtoFTP, Username: "me"}, true},
		{store.Server{Protocol: store.ProtoFTP, Username: "me", TLS: true}, false},
		{store.Server{Protocol: store.ProtoFTP, Anonymous: true}, false},
		{store.Server{Protocol: store.ProtoSFTP, Username: "me"}, false},
		{store.Server{Protocol: store.ProtoSMB, Username: "me"}, false},
	} {
		if got := plainFTPLogin(c.srv); got != c.want {
			t.Errorf("%+v: %v, want %v", c.srv, got, c.want)
		}
	}
}
