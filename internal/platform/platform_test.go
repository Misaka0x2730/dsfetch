package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testPlatform(t *testing.T) *Platform {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "mnt/mmc/Roms/nds"), 0o755)
	os.MkdirAll(filepath.Join(root, "mnt/mmc/Roms/GBA"), 0o755)
	os.MkdirAll(filepath.Join(root, "mnt/sdcard/Roms/PS"), 0o755)
	p, err := Load(Options{Root: root, Dev: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSuggestSystem(t *testing.T) {
	p := testPlatform(t)
	cases := []struct{ file, dir, want string }{
		{"Pokemon Platinum (USA).nds", "/", "NDS"},
		{"Pokemon Platinum (USA).nds.7z", "/", "NDS"},
		{"Metroid Fusion.GBA", "/", "GBA"},
		{"Game.7z", "/roms/snes/", "SFC"},
		{"Game.zip", "/share/Games/Nintendo DS", "NDS"},
		{"Crash.chd", "/roms/psx", "PS"},
		{"file.bin", "/misc", ""},
	}
	for _, c := range cases {
		got, ok := p.SuggestSystem(c.file, c.dir)
		if (c.want == "") != !ok || got.Dir != c.want {
			t.Errorf("SuggestSystem(%q, %q) = %q, %v; want %q", c.file, c.dir, got.Dir, ok, c.want)
		}
	}
}

func TestExistingDirForKeepsCase(t *testing.T) {
	p := testPlatform(t)
	cards := p.AvailableCards()
	if len(cards) != 2 {
		t.Fatalf("cards = %+v", cards)
	}
	nds, _ := p.SystemByDir("NDS")
	if got := p.ExistingDirFor(cards[0], nds); got != "nds" {
		t.Fatalf("ExistingDirFor = %q, want existing lowercase folder", got)
	}
	gb, _ := p.SystemByDir("GB")
	if got := p.ExistingDirFor(cards[0], gb); got != "GB" {
		t.Fatalf("ExistingDirFor = %q, want canonical name", got)
	}
	if got := p.RomDirs(cards); len(got) != 3 || got[0] != "GBA" || got[1] != "nds" || got[2] != "PS" {
		t.Fatalf("RomDirs = %v", got)
	}
}

func TestPlatformOverride(t *testing.T) {
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "platform.json"), []byte(`{"roms_dir": "roms", "cards": [{"id":"sd","label":"SD","path":"/storage"}]}`), 0o644)
	p, err := Load(Options{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if p.RomsDir != "roms" || len(p.Cards) != 1 || p.Cards[0].Path != "/storage" {
		t.Fatalf("override not applied: %+v", p.Config)
	}
	if len(p.Systems) == 0 {
		t.Fatal("systems from defaults lost")
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		"Zelda: Link's Awakening.gb": "Zelda_ Link's Awakening.gb",
		`a*b?c"d<e>f|g\h`:            "a_b_c_d_e_f_g_h",
		"trailing. . ":               "trailing",
		"Покемон.nds":                "Покемон.nds",
		"..":                         "_",
	}
	for in, want := range cases {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckSpace(t *testing.T) {
	dir := t.TempDir()
	if err := CheckSpace(dir, 1024, 1024); err != nil {
		t.Fatal(err)
	}
	var ns *NoSpaceError
	if err := CheckSpace(filepath.Join(dir, "not", "yet"), 1<<62, 1); err == nil {
		t.Fatal("expected no-space error")
	} else if !asNoSpace(err, &ns) {
		t.Fatalf("err = %v", err)
	}
}

func asNoSpace(err error, target **NoSpaceError) bool {
	e, ok := err.(*NoSpaceError)
	if ok {
		*target = e
	}
	return ok
}

func TestKeepsArchives(t *testing.T) {
	p := testPlatform(t)
	for rel, want := range map[string]bool{"Roms/ARCADE": true, "Roms/mame": true, "Roms/NEOGEO": true, "Roms/NDS": false, "Roms/Other": false} {
		if got := p.KeepsArchives(rel); got != want {
			t.Errorf("KeepsArchives(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestInhibitSleepNests(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	p := &Platform{Config: Config{
		InhibitSleepCmd: "echo inhibit >> " + log,
		AllowSleepCmd:   "echo allow >> " + log,
	}}
	outer := p.InhibitSleep()
	inner := p.InhibitSleep()
	inner()
	inner() // releasing twice must not count twice
	if data, _ := os.ReadFile(log); string(data) != "inhibit\n" {
		t.Fatalf("after inner release: %q", data)
	}
	outer()
	if data, _ := os.ReadFile(log); string(data) != "inhibit\nallow\n" {
		t.Fatalf("after outer release: %q", data)
	}
}

func TestSystemLanguage(t *testing.T) {
	p := testPlatform(t)
	if got := p.SystemLanguage(); got != "" {
		t.Fatalf("no language.ini: %q, want empty", got)
	}
	file := filepath.Join(p.Root, "mnt/vendor/oem/language.ini")
	os.MkdirAll(filepath.Dir(file), 0o755)
	for content, want := range map[string]string{"6\n": "ru", "2": "en", " 7 \r\n1\n": "de", "1\n": "zh-Hant", "42\n": "", "x\n": ""} {
		os.WriteFile(file, []byte(content), 0o644)
		if got := p.SystemLanguage(); got != want {
			t.Errorf("language.ini %q: %q, want %q", content, got, want)
		}
	}
}

func TestScreensSwapped(t *testing.T) {
	p := testPlatform(t)
	if p.ScreensSwapped() {
		t.Fatal("no lcdswap file: swapped")
	}
	file := filepath.Join(p.Root, "sys/class/anbernic_misc/lcdswap")
	os.MkdirAll(filepath.Dir(file), 0o755)
	for content, want := range map[string]bool{"1\n": true, "1": true, "0\n": false, "": false} {
		os.WriteFile(file, []byte(content), 0o644)
		if got := p.ScreensSwapped(); got != want {
			t.Errorf("lcdswap %q: %v, want %v", content, got, want)
		}
	}
}

func TestSuggestSystemLooksThroughRar(t *testing.T) {
	p := testPlatform(t)
	for _, file := range []string{"Game.nds.rar", "Game.nds.part1.rar", "Game.nds.part02.rar", "Game.nds.7z.001"} {
		if got, ok := p.SuggestSystem(file, "/"); !ok || got.Dir != "NDS" {
			t.Errorf("SuggestSystem(%q) = %q, %v; want NDS", file, got.Dir, ok)
		}
	}
}

// DSFETCH_FAKE_FREE only changes the displayed free space, and only in dev
// mode.
func TestFreeSpaceOverride(t *testing.T) {
	p := testPlatform(t) // dev mode
	cards := p.AvailableCards()
	if len(cards) < 2 {
		t.Fatalf("cards = %v", cards)
	}
	real, ok := p.FreeSpace(cards[0])
	if !ok || real == 0 {
		t.Fatalf("real free space = %d, %v", real, ok)
	}
	t.Setenv("DSFETCH_FAKE_FREE", "tf1=2.78G, tf2=25.2GB")
	gib := float64(1 << 30)
	fake := map[string]uint64{"tf1": uint64(2.78 * gib), "tf2": uint64(25.2 * gib)}
	for _, c := range cards {
		if got, _ := p.FreeSpace(c); got != fake[c.ID] {
			t.Errorf("%s: %d, want %d", c.ID, got, fake[c.ID])
		}
	}
	// Not compared with the reading above: anything writing to this disk
	// meanwhile changes it. A real reading is whole blocks, never these values.
	p.Dev = false
	for _, c := range cards {
		if got, ok := p.FreeSpace(c); !ok || got == fake[c.ID] {
			t.Errorf("%s: override applied outside dev mode: %d, %v", c.ID, got, ok)
		}
	}
}

func TestDeviceID(t *testing.T) {
	p := testPlatform(t)
	p.DeviceIDFile = "/sys/serial-number"
	if id := p.DeviceID(); id != nil {
		t.Fatalf("missing file: %q", id)
	}
	file := filepath.Join(p.Root, "sys/serial-number")
	os.MkdirAll(filepath.Dir(file), 0o755)
	for content, want := range map[string]string{
		"c3d9b8674f4b94f6\x00": "c3d9b8674f4b94f6", // device-tree strings end in NUL
		"0000000000000000\x00": "",                 // unset OTP
		"\n":                   "",
	} {
		os.WriteFile(file, []byte(content), 0o644)
		if got := string(p.DeviceID()); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
}

func TestSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o755)
	if same, err := SameFilesystem(dir, sub); !same || err != nil {
		t.Errorf("SameFilesystem(dir, dir/sub) = %v, %v", same, err)
	}
	other := "/proc" // Linux
	if runtime.GOOS == "darwin" {
		other = "/dev"
	}
	if same, err := SameFilesystem(dir, other); same || err != nil {
		t.Errorf("SameFilesystem(%s, %s) = %v, %v", dir, other, same, err)
	}
	if _, err := SameFilesystem(dir, filepath.Join(dir, "missing")); err == nil {
		t.Error("no error for a missing path")
	}
}

// A stray variable must not change a normal run's memory checks.
func TestFakeMemAvailableOnlyInDevMode(t *testing.T) {
	t.Setenv("DSFETCH_FAKE_MEMAVAILABLE", "12345")
	SetDevOverrides(false)
	if n, ok := MemAvailable(); ok && n == 12345 {
		t.Error("override used outside dev mode")
	}
	SetDevOverrides(true)
	t.Cleanup(func() { SetDevOverrides(false) })
	if n, ok := MemAvailable(); !ok || n != 12345 {
		t.Errorf("MemAvailable = %d, %v in dev mode", n, ok)
	}
}

// A damaged platform.json must not stop DSFetch: it is kept as .bad and the
// built-in values are used, also when only a type was wrong.
func TestDamagedOverrideIsSetAside(t *testing.T) {
	defaults, err := Load(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"syntax": `{"cards": [`,
		"type":   `{"cards": [{"id": "x", "label": 1}], "roms_dir": "Games"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "platform.json"), []byte(content), 0o644)
			p, err := Load(Options{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.SetAside) != 1 || p.SetAside[0] != "platform.json" {
				t.Fatalf("SetAside = %v", p.SetAside)
			}
			if p.RomsDir != defaults.RomsDir || len(p.Cards) != len(defaults.Cards) || p.Cards[0] != defaults.Cards[0] {
				t.Fatalf("config not the defaults: roms %q, cards %+v", p.RomsDir, p.Cards)
			}
			if kept, _ := os.ReadFile(filepath.Join(dir, "platform.json.bad")); string(kept) != content {
				t.Fatalf(".bad holds %q", kept)
			}
		})
	}
}
