package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/atomic"
	"golang.org/x/text/encoding/charmap"

	"dsfetch/internal/platform"
)

const fixtures = "../../testdata/archives"

func romContent() []byte {
	var b strings.Builder
	for i := 0; i < 4096; i++ {
		fmt.Fprintf(&b, "ROM%05d", i)
	}
	return []byte(b.String())
}

// tree lists every file under root as "rel=size" (slash paths, sorted).
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		st, _ := os.Stat(p)
		out = append(out, fmt.Sprintf("%s=%d", filepath.ToSlash(rel), st.Size()))
		return nil
	})
	sort.Strings(out)
	return out
}

func assertTree(t *testing.T, root string, want ...string) {
	t.Helper()
	got := tree(t, root)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tree mismatch\n got: %v\nwant: %v", got, want)
	}
}

func assertNoTemp(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		for _, p := range []string{d, filepath.Dir(d)} {
			if _, err := os.Stat(filepath.Join(p, TempDirName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s left behind in %s: %v", TempDirName, p, err)
			}
		}
	}
}

// ---------------------------------------------------------------- zip

type zipFile struct {
	name     string
	raw      bool // name is raw bytes, UTF-8 flag not set
	data     []byte
	method   uint16
	extra    []byte
	mode     fs.FileMode
	flags    uint16
	fakeSize uint64
}

func makeZip(t *testing.T, files ...zipFile) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: f.method, Extra: f.extra, NonUTF8: f.raw, Modified: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)}
		if f.mode != 0 {
			h.SetMode(f.mode)
		}
		if f.flags != 0 || f.fakeSize != 0 {
			// Raw entry: lets the test forge flags and sizes.
			h.Flags |= f.flags
			h.CRC32 = crc32.ChecksumIEEE(f.data)
			h.CompressedSize64 = uint64(len(f.data))
			h.UncompressedSize64 = uint64(len(f.data))
			if f.fakeSize != 0 {
				h.UncompressedSize64 = f.fakeSize
			}
			h.Method = zip.Store
			wr, err := w.CreateRaw(h)
			if err != nil {
				t.Fatal(err)
			}
			wr.Write(f.data)
			continue
		}
		wr, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		wr.Write(f.data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "test.zip")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func extractZip(t *testing.T, path string, flatten bool) (string, error) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "Roms", "NDS")
	z := &Zip{Options: Options{FlattenSingleDir: flatten}}
	err := z.Extract(context.Background(), path, dst, nil)
	return dst, err
}

func TestZipStoreAndDeflate(t *testing.T) {
	rom := romContent()
	p := makeZip(t,
		zipFile{name: "game.nds", data: rom, method: zip.Store},
		zipFile{name: "docs/readme.txt", data: []byte("hi"), method: zip.Deflate},
		zipFile{name: "docs/", mode: fs.ModeDir | 0o755},
	)
	dst, err := extractZip(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, fmt.Sprintf("game.nds=%d", len(rom)), "docs/readme.txt=2")
	got, _ := os.ReadFile(filepath.Join(dst, "game.nds"))
	if !bytes.Equal(got, rom) {
		t.Fatal("content mismatch")
	}
	assertNoTemp(t, dst)

	files, size, err := (&Zip{}).List(p)
	if err != nil || len(files) != 2 || size != int64(len(rom)+2) {
		t.Fatalf("List = %v %d %v", files, size, err)
	}
}

func TestZipCP866Names(t *testing.T) {
	enc := charmap.CodePage866.NewEncoder()
	dir, _ := enc.String("Игры")
	file, _ := enc.String("Тетрис.gb")
	p := makeZip(t, zipFile{name: dir + "/" + file, raw: true, data: []byte("tetris"), method: zip.Deflate})
	dst, err := extractZip(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Игры/Тетрис.gb=6")
}

func TestZipCP437Fallback(t *testing.T) {
	// 0xC4 (box drawing) never occurs in CP866 letters, so CP437 wins;
	// 0x82 is "é" in CP437.
	p := makeZip(t, zipFile{name: "Caf\x82\xc4.txt", raw: true, data: []byte("x"), method: zip.Store})
	dst, err := extractZip(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Café─.txt=1")
}

func TestZipUnicodePathExtra(t *testing.T) {
	raw := "Igry.txt" // what an old archiver put in the header
	unicode := "Игры.txt"
	extra := make([]byte, 4+5+len(unicode))
	binary.LittleEndian.PutUint16(extra[0:], 0x7075)
	binary.LittleEndian.PutUint16(extra[2:], uint16(5+len(unicode)))
	extra[4] = 1
	binary.LittleEndian.PutUint32(extra[5:], crc32.ChecksumIEEE([]byte(raw)))
	copy(extra[9:], unicode)
	// The raw name needs a high byte for the extra field to be consulted.
	raw = "Igry\xff.txt"
	binary.LittleEndian.PutUint32(extra[5:], crc32.ChecksumIEEE([]byte(raw)))
	p := makeZip(t, zipFile{name: raw, raw: true, data: []byte("u"), method: zip.Store, extra: extra})
	dst, err := extractZip(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Игры.txt=1")
}

func TestZipUTF8WithoutFlag(t *testing.T) {
	p := makeZip(t, zipFile{name: "Покемон.nds", raw: true, data: []byte("p"), method: zip.Store})
	dst, err := extractZip(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Покемон.nds=1")
}

func TestZipSlipRejected(t *testing.T) {
	for _, evil := range []string{"../evil.txt", "a/../../evil.txt", "/etc/evil.txt", `..\evil.txt`, `C:\evil.txt`} {
		t.Run(evil, func(t *testing.T) {
			p := makeZip(t,
				zipFile{name: "good.txt", data: []byte("ok"), method: zip.Store},
				zipFile{name: evil, raw: true, data: []byte("pwned"), method: zip.Store},
			)
			base := t.TempDir()
			dst := filepath.Join(base, "Roms", "NDS")
			err := (&Zip{}).Extract(context.Background(), p, dst, nil)
			var unsafe *UnsafePathError
			if !errors.As(err, &unsafe) {
				t.Fatalf("err = %v, want UnsafePathError", err)
			}
			// Nothing at all may be written, not even the good file.
			if got := tree(t, base); len(got) != 0 {
				t.Fatalf("files written: %v", got)
			}
		})
	}
}

func TestZipEncryptedRejected(t *testing.T) {
	p := makeZip(t, zipFile{name: "secret.nds", data: []byte("xxxx"), flags: flagEncrypted})
	if _, err := extractZip(t, p, false); !errors.Is(err, ErrEncryptedZip) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Inspect(p, Options{}); !errors.Is(err, ErrEncryptedZip) {
		t.Fatalf("inspect err = %v", err)
	}
}

func TestZipJunkAndSymlinksSkipped(t *testing.T) {
	p := makeZip(t,
		zipFile{name: "Game/Game.nds", data: []byte("rom"), method: zip.Store},
		zipFile{name: "Game/.DS_Store", data: []byte("j"), method: zip.Store},
		zipFile{name: "Game/Thumbs.db", data: []byte("j"), method: zip.Store},
		zipFile{name: "__MACOSX/Game/._Game.nds", data: []byte("j"), method: zip.Store},
		zipFile{name: "Game/._Game.nds", data: []byte("j"), method: zip.Store},
		zipFile{name: "Game/link", data: []byte("/etc/passwd"), method: zip.Store, mode: fs.ModeSymlink | 0o777},
	)
	// __MACOSX is junk, so "Game" is the single top folder and is flattened.
	dst, err := extractZip(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Game.nds=3")
}

func TestZipMergeAndOverwrite(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "PS")
	os.MkdirAll(filepath.Join(dst, "Game"), 0o755)
	os.WriteFile(filepath.Join(dst, "Game", "old.sav"), []byte("save"), 0o644)
	os.WriteFile(filepath.Join(dst, "Game", "game.cue"), []byte("old cue"), 0o644)
	p := makeZip(t,
		zipFile{name: "Game/game.cue", data: []byte("new"), method: zip.Store},
		zipFile{name: "Game/game.bin", data: []byte("binary"), method: zip.Store},
		zipFile{name: "other.txt", data: []byte("o"), method: zip.Store},
	)
	if err := (&Zip{}).Extract(context.Background(), p, dst, nil); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Game/old.sav=4", "Game/game.cue=3", "Game/game.bin=6", "other.txt=1")
}

func TestZipNoSpace(t *testing.T) {
	p := makeZip(t, zipFile{name: "huge.iso", data: []byte("tiny"), fakeSize: 1 << 60})
	_, err := extractZip(t, p, false)
	var ns *platform.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("err = %v, want NoSpaceError", err)
	}
}

func TestExtractCancelLeavesNothing(t *testing.T) {
	big := bytes.Repeat([]byte("abcdefgh"), 4<<20) // 32 MB, several copy loops
	p := makeZip(t, zipFile{name: "big.bin", data: big, method: zip.Store})
	base := t.TempDir()
	dst := filepath.Join(base, "Roms", "PSP")
	ctx, cancel := context.WithCancel(context.Background())
	progress := atomic.NewFloat64(0)
	go func() {
		for progress.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	err := (&Zip{}).Extract(ctx, p, dst, progress)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := tree(t, base); len(got) != 0 {
		t.Fatalf("partial files left: %v", got)
	}
}

// ---------------------------------------------------------------- 7z

func extract7z(t *testing.T, name, password string, flatten bool) (string, error) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "Roms", "NDS")
	ex, err := For(filepath.Join(fixtures, name), Options{Password: password, FlattenSingleDir: flatten})
	if err != nil {
		t.Fatal(err)
	}
	progress := atomic.NewFloat64(0)
	err = ex.Extract(context.Background(), filepath.Join(fixtures, name), dst, progress)
	if err == nil && progress.Load() != 1 {
		t.Fatalf("progress = %v", progress.Load())
	}
	return dst, err
}

var standardTree = []string{"game.nds=32768", "readme.txt=19", "sub/nested.txt=7"}

func TestSevenZipMethods(t *testing.T) {
	for _, name := range []string{"lzma.7z", "lzma2.7z", "bcj2.7z", "ppmd.7z", "solid.7z", "nonsolid.7z"} {
		t.Run(name, func(t *testing.T) {
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

func TestSevenZipMultiVolume(t *testing.T) {
	dst, err := extract7z(t, "multivolume.7z.001", "", true)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "noise.bin=30000")
	vols := Volumes(filepath.Join(fixtures, "multivolume.7z.001"))
	if len(vols) != 3 {
		t.Fatalf("volumes = %v", vols)
	}
}

func TestSevenZipEncrypted(t *testing.T) {
	for _, name := range []string{"encrypted.7z", "encrypted-headers.7z"} {
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

func TestSevenZipFlattenAndJunk(t *testing.T) {
	dst, err := extract7z(t, "singledir.7z", "", true)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Pokemon Platinum (USA).nds=32768")

	dst, err = extract7z(t, "singledir.7z", "", false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Pokemon Platinum (USA)/Pokemon Platinum (USA).nds=32768")
}

func TestSevenZipCyrillic(t *testing.T) {
	dst, err := extract7z(t, "cyrillic.7z", "", false)
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Игры/Тетрис.gb=8")
}

func TestSevenZipHeaderMemory(t *testing.T) {
	cases := map[string]int64{
		"lzma.7z":            48 << 10,
		"lzma2.7z":           48 << 10,
		"bcj2.7z":            48 << 10,
		"nonsolid.7z":        32 << 10,
		"ppmd.7z":            1 << 20,
		"bigdict.7z":         64 << 20,
		"encrypted.7z":       48 << 10, // data encrypted, header readable
		"multivolume.7z.001": 0,        // stored (-mx=0): copy coder
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := SevenZipDecoderMemory(filepath.Join(fixtures, name))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("decoder memory = %d, want %d", got, want)
			}
		})
	}

	h, err := ReadSevenZipHeader(filepath.Join(fixtures, "encrypted-headers.7z"))
	if err != nil {
		t.Fatal(err)
	}
	if !h.HeaderEncrypted || h.DecoderMemory() != 0 {
		t.Fatalf("encrypted headers: %+v", h)
	}
	h, _ = ReadSevenZipHeader(filepath.Join(fixtures, "encrypted.7z"))
	if h.HeaderEncrypted || !h.DataEncrypted {
		t.Fatalf("encrypted data: %+v", h)
	}
	info, err := Inspect(filepath.Join(fixtures, "bigdict.7z"), Options{})
	if err != nil || info.DecoderMemory != 64<<20 || info.UnpackedSize != 70_000_000 {
		t.Fatalf("inspect bigdict = %+v, %v", info, err)
	}
}

func TestSevenZipCLI(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("7zz not installed")
	}
	run := func(name, password string) (string, error) {
		dst := filepath.Join(t.TempDir(), "Roms", "NDS")
		ex := &SevenZipCLI{Options: Options{Password: password, FlattenSingleDir: true, SevenZipBinary: bin}}
		progress := atomic.NewFloat64(0)
		err := ex.Extract(context.Background(), filepath.Join(fixtures, name), dst, progress)
		return dst, err
	}
	dst, err := run("bcj2.7z", "")
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, standardTree...)
	assertNoTemp(t, dst)

	dst, err = run("singledir.7z", "")
	if err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, "Pokemon Platinum (USA).nds=32768") // junk removed, flattened

	if _, err := run("encrypted.7z", ""); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("no password: %v", err)
	}
	if _, err := run("encrypted.7z", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := run("encrypted.7z", "Secret123"); err != nil {
		t.Fatal(err)
	}
}

func TestKind(t *testing.T) {
	for name, want := range map[string]string{
		"a.zip": "zip", "A.ZIP": "zip", "b.7z": "7z", "c.7z.001": "7z", "d.7z.002": "", "e.nds": "",
	} {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}
