package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.uber.org/atomic"

	"dsfetch/internal/platform"
)

// RAR fixtures come from scripts/make-rar-fixtures.sh (RARLAB rar in Docker).

func TestRarFormats(t *testing.T) {
	for _, name := range []string{"rar5.rar", "rar4.rar", "solid.rar"} {
		t.Run(name, func(t *testing.T) {
			if ex, _ := For(filepath.Join(fixtures, name), Options{}); ex == nil {
				t.Fatal("no extractor")
			} else if _, ok := ex.(*Rar); !ok {
				t.Fatalf("extractor = %T, want *Rar", ex)
			}
			dst, err := extract7z(t, name, "", true)
			if err != nil {
				t.Fatal(err)
			}
			assertTree(t, dst, standardTree...)
			got, _ := os.ReadFile(filepath.Join(dst, "game.nds"))
			if !bytes.Equal(got, romContent()) {
				t.Fatal("game.nds content mismatch")
			}
			assertNoTemp(t, dst)
		})
	}
}

func TestRarMultiVolume(t *testing.T) {
	for _, name := range []string{"multivolume.part1.rar", "multivolume-old.rar"} {
		t.Run(name, func(t *testing.T) {
			dst, err := extract7z(t, name, "", true)
			if err != nil {
				t.Fatal(err)
			}
			assertTree(t, dst, "noise.bin=30000")
			if vols := Volumes(filepath.Join(fixtures, name)); len(vols) != 3 {
				t.Fatalf("volumes = %v", vols)
			}
		})
	}
}

func TestRarEncrypted(t *testing.T) {
	for _, name := range []string{"encrypted.rar", "encrypted-rar4.rar", "encrypted-headers.rar"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(fixtures, name)
			if _, err := Inspect(path, Options{}); !errors.Is(err, ErrPasswordRequired) {
				t.Fatalf("inspect without password: %v", err)
			}
			if _, err := extract7z(t, name, "", true); !errors.Is(err, ErrPasswordRequired) {
				t.Fatalf("extract without password: %v", err)
			}
			if _, err := extract7z(t, name, "wrong", true); !errors.Is(err, ErrWrongPassword) {
				t.Fatalf("extract with wrong password: %v", err)
			}
			dst, err := extract7z(t, name, "Secret123", true)
			if err != nil {
				t.Fatal(err)
			}
			assertTree(t, dst, "game.nds=32768", "readme.txt=19")
		})
	}
}

func TestRarCyrillic(t *testing.T) {
	dst, err := extract7z(t, "cyrillic.rar", "", false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Игры/Тетрис.gb=8")
}

func TestRarDecoderMemory(t *testing.T) {
	info, err := Inspect(filepath.Join(fixtures, "bigdict.rar"), Options{})
	if err != nil || info.DecoderMemory != 64<<20 || info.UnpackedSize != 70_000_000 {
		t.Fatalf("inspect bigdict = %+v, %v", info, err)
	}
	// A dictionary larger than the file is capped at the file's size.
	if mem, err := RarDecoderMemory(filepath.Join(fixtures, "rar5.rar")); err != nil || mem != 32768 {
		t.Fatalf("rar5 = %d, %v, want 32768", mem, err)
	}
	if mem, err := RarDecoderMemory(filepath.Join(fixtures, "rar4.rar")); err != nil || mem != 0 {
		t.Fatalf("rar4 = %d, %v, want 0", mem, err)
	}
	if mem, err := RarDecoderMemory(filepath.Join(fixtures, "encrypted-headers.rar")); err != nil || mem != 0 {
		t.Fatalf("encrypted headers = %d, %v, want 0", mem, err)
	}
}

// A dictionary that does not fit in free memory is refused, not allocated.
func TestRarDictionaryTooLarge(t *testing.T) {
	t.Setenv("DSFETCH_FAKE_MEMAVAILABLE", "100000000") // ~36 MB left for the dictionary
	platform.SetDevOverrides(true)
	t.Cleanup(func() { platform.SetDevOverrides(false) })
	if _, err := extract7z(t, "bigdict.rar", "", true); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("err = %v, want ErrOutOfMemory", err)
	}
}

func TestRarCLI(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("7zz not installed")
	}
	run := func(name, password string) (string, error) {
		dst := filepath.Join(t.TempDir(), "Roms", "NDS")
		ex, err := For(filepath.Join(fixtures, name), Options{Password: password, FlattenSingleDir: true, SevenZipBinary: bin})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ex.(*SevenZipCLI); !ok {
			t.Fatalf("extractor = %T, want *SevenZipCLI", ex)
		}
		return dst, ex.Extract(context.Background(), filepath.Join(fixtures, name), dst, atomic.NewFloat64(0))
	}
	for _, name := range []string{"rar5.rar", "rar4.rar", "solid.rar"} {
		dst, err := run(name, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertTree(t, dst, standardTree...)
		assertNoTemp(t, dst)
	}
	for _, name := range []string{"multivolume.part1.rar", "multivolume-old.rar"} {
		dst, err := run(name, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertTree(t, dst, "noise.bin=30000")
	}
	for _, name := range []string{"encrypted.rar", "encrypted-headers.rar"} {
		if _, err := run(name, ""); !errors.Is(err, ErrPasswordRequired) {
			t.Fatalf("%s without password: %v", name, err)
		}
		if _, err := run(name, "wrong"); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("%s with wrong password: %v", name, err)
		}
		if _, err := run(name, "Secret123"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRarKindAndVolume(t *testing.T) {
	for name, want := range map[string]string{
		"a.rar": "rar", "A.RAR": "rar", "b.part1.rar": "rar", "b.part01.rar": "rar",
		"b.part2.rar": "", "b.part10.rar": "", "c.r00": "", "c.s01": "",
	} {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
	type vol struct{ first, key string }
	for path, want := range map[string]vol{
		"/r/Game.zip":           {"/r/Game.zip", "/r/game.zip"},
		"/r/Game.rar":           {"/r/Game.rar", "/r/game.rar"},
		"/r/Game.r00":           {"/r/Game.rar", "/r/game.rar"},
		"/r/Game.R07":           {"/r/Game.RAR", "/r/game.rar"},
		"/r/Game.s01":           {"/r/Game.rar", "/r/game.rar"},
		"/r/Game.part1.rar":     {"/r/Game.part1.rar", "/r/game.part.rar"},
		"/r/Game.part03.rar":    {"/r/Game.part01.rar", "/r/game.part.rar"},
		"/r/Game.7z.001":        {"/r/Game.7z.001", "/r/game.7z"},
		"/r/Game.7z.002":        {"/r/Game.7z.001", "/r/game.7z"},
		"/r/Game.nds.part2.rar": {"/r/Game.nds.part1.rar", "/r/game.nds.part.rar"},
	} {
		first, key, ok := Volume(path)
		if !ok || first != want.first || key != want.key {
			t.Errorf("Volume(%q) = %q, %q, %v; want %q, %q", path, first, key, ok, want.first, want.key)
		}
	}
	if _, _, ok := Volume("/r/readme.txt"); ok {
		t.Error("readme.txt is not an archive")
	}
}
