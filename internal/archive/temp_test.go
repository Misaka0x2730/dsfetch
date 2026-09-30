package archive

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTempDirOnCardRoot(t *testing.T) {
	card := t.TempDir()
	dst := filepath.Join(card, "Roms", "NDS")

	tmp, cleanup, err := tempDirFor(dst, card)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(card, TempDirName) + string(filepath.Separator); !strings.HasPrefix(tmp, want) {
		t.Errorf("temp dir %s, want it under %s", tmp, want)
	}
	cleanup()
	assertNoTemp(t, card)

	// No card known: inside the target itself.
	tmp, cleanup, err = tempDirFor(dst, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dst, TempDirName) + string(filepath.Separator); !strings.HasPrefix(tmp, want) {
		t.Errorf("temp dir %s, want it under %s", tmp, want)
	}
	cleanup()
	assertNoTemp(t, dst)
}

// "Unpack here" for an archive in the root of a card: the old temp folder
// next to the target was the card's parent, on another filesystem.
func TestExtractIntoCardRoot(t *testing.T) {
	p := makeZip(t,
		zipFile{name: "game.nds", data: []byte("rom"), method: zip.Store},
		zipFile{name: TempDirName + "/junk.txt", data: []byte("x"), method: zip.Store},
	)
	card := filepath.Join(t.TempDir(), "sdcard")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	z := &Zip{Options: Options{TempRoot: card}}
	if err := z.Extract(context.Background(), p, card, nil); err != nil {
		t.Fatal(err)
	}
	assertTree(t, card, "game.nds=3")
	assertNoTemp(t, card)
}

func TestRemoveStaleTemp(t *testing.T) {
	card := t.TempDir()
	left := filepath.Join(card, TempDirName, "x-123", "Game")
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(left, "half.iso"), []byte("partial"), 0o644)

	if found, err := RemoveStaleTemp(card); !found || err != nil {
		t.Fatalf("RemoveStaleTemp = %v, %v", found, err)
	}
	assertNoTemp(t, card)
	if found, err := RemoveStaleTemp(card); found || err != nil {
		t.Fatalf("second RemoveStaleTemp = %v, %v", found, err)
	}
}

// FAT refuses " * : < > ? \ | and names ending in a dot or space (EINVAL).
func TestNamesMadeValidForFAT(t *testing.T) {
	p := makeZip(t,
		zipFile{name: "Zelda: Link's Awakening?.gb", data: []byte("1"), method: zip.Store},
		zipFile{name: "Zelda_ Link's Awakening_.gb", data: []byte("22"), method: zip.Store},
		zipFile{name: "Disc <1>/track|01.bin", data: []byte("333"), method: zip.Store},
		zipFile{name: "readme. ", data: []byte("4444"), method: zip.Store},
	)
	dst, err := extractZip(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst,
		"Zelda_ Link's Awakening_.gb=2", // the unchanged name keeps its file
		"Zelda_ Link's Awakening_ (2).gb=1",
		"Disc _1_/track_01.bin=3",
		"readme=4",
	)
}

// 7zz on Linux writes such names as they are, so the built-in decoder
// unpacks those archives.
func TestSevenZipCLIHandsUnsafeNamesToBuiltin(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("7zz not installed")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Game: Special?.nds"), []byte("rom"), 0o644); err != nil {
		t.Skipf("this filesystem refuses the name: %v", err)
	}
	arc := filepath.Join(t.TempDir(), "names.7z")
	if out, err := exec.Command(bin, "a", "-bso0", arc, filepath.Join(src, "Game: Special?.nds")).CombinedOutput(); err != nil {
		t.Fatalf("7zz a: %v: %s", err, out)
	}
	dst := filepath.Join(t.TempDir(), "Roms", "NDS")
	ex := &SevenZipCLI{Options: Options{SevenZipBinary: bin}}
	if err := ex.Extract(context.Background(), arc, dst, nil); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Game_ Special_.nds=3")
	assertNoTemp(t, dst)
}
