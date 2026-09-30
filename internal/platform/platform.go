// Package platform describes the device: where the memory cards are mounted,
// which system folders live under Roms, and device-specific hooks (sleep
// inhibition, frontend rescan). Defaults for the RG DS Plus are embedded and
// can be overridden with <data>/platform.json without rebuilding.
package platform

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

//go:embed rgdsplus.json
var defaultConfigJSON []byte

// Card is a memory card mount point.
type Card struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
}

// System is a folder under Roms that belongs to an emulated system.
type System struct {
	Dir        string   `json:"dir"`
	Name       string   `json:"name"`
	Extensions []string `json:"ext,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
	// KeepArchives: the emulator loads .zip as-is (MAME, FBNeo, Neo Geo),
	// so archives must not be unpacked.
	KeepArchives bool `json:"keep_archives,omitempty"`
}

// Config is the JSON-serialisable device description.
type Config struct {
	Name    string   `json:"name"`
	Cards   []Card   `json:"cards"`
	RomsDir string   `json:"roms_dir"`
	Systems []System `json:"systems"`

	// Shell commands run around long transfers. Empty = do nothing. The right
	// mechanism for the stock firmware is determined in Stage 0.
	InhibitSleepCmd string `json:"inhibit_sleep_cmd,omitempty"`
	AllowSleepCmd   string `json:"allow_sleep_cmd,omitempty"`

	// The system language setting: a file whose first line is an index
	// into LanguageCodes (the stock firmware's /mnt/vendor/oem/language.ini).
	LanguageFile  string   `json:"language_file,omitempty"`
	LanguageCodes []string `json:"language_codes,omitempty"`

	// The system "swap screens" setting: a file that reads 1 when the main
	// screen is display 1 instead of 0 (the stock firmware's lcdswap).
	ScreenSwapFile string `json:"screen_swap_file,omitempty"`

	// The lid: LidFile reads LidClosedValue while the lid is shut (the stock
	// firmware's hall sensor), and SleepCmd suspends the console the way the
	// system does on a lid close. Opening the lid wakes it up.
	LidFile        string `json:"lid_file,omitempty"`
	LidClosedValue string `json:"lid_closed_value,omitempty"`
	SleepCmd       string `json:"sleep_cmd,omitempty"`

	// The system sleep timer: a little-endian int32 at SleepTimerOffset of
	// SleepTimerFile (only if the file is SleepTimerFileSize bytes, when set)
	// that indexes SleepTimerSeconds; 0 seconds means never.
	SleepTimerFile     string `json:"sleep_timer_file,omitempty"`
	SleepTimerFileSize int    `json:"sleep_timer_file_size,omitempty"`
	SleepTimerOffset   int    `json:"sleep_timer_offset,omitempty"`
	SleepTimerSeconds  []int  `json:"sleep_timer_seconds,omitempty"`

	// DeviceIDFile holds an ID unique to the console (the stock firmware's
	// CPU serial in the device tree); saved passwords are bound to it.
	DeviceIDFile string `json:"device_id_file,omitempty"`
}

// Platform is the resolved runtime description.
type Platform struct {
	Config
	// Root is prepended to every card path (the -root dev flag).
	Root    string
	DataDir string
	Dev     bool
	// SetAside names the damaged files Load renamed to .bad (platform.json).
	SetAside []string

	sleepMu    sync.Mutex
	sleepHolds int
	lidSleep   atomic.Bool
	idleSleep  atomic.Bool
	lastActive atomic.Int64 // unix nanoseconds of the last input or work
}

// Options for Load.
type Options struct {
	Root    string // filesystem prefix for card paths ("" on the device)
	DataDir string // app data directory (servers.json, logs, overrides)
	Dev     bool
}

// Load builds the platform from embedded defaults plus an optional
// <DataDir>/platform.json override. A damaged override is renamed to .bad
// and the defaults are used (see Platform.SetAside).
func Load(opts Options) (*Platform, error) {
	var cfg Config
	if err := json.Unmarshal(defaultConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("embedded platform config: %w", err)
	}
	var setAside []string
	if opts.DataDir != "" {
		override := filepath.Join(opts.DataDir, "platform.json")
		if data, err := os.ReadFile(override); err == nil {
			// Unmarshal over the defaults so the override may be partial.
			if err := json.Unmarshal(data, &cfg); err != nil {
				cause := fmt.Errorf("%s: %w", override, err)
				if os.Rename(override, override+".bad") != nil {
					return nil, cause
				}
				slog.Warn("damaged file renamed", "to", override+".bad", "err", cause)
				setAside = append(setAside, "platform.json")
				// A type error leaves cfg (and its slices) half overwritten.
				cfg = Config{}
				_ = json.Unmarshal(defaultConfigJSON, &cfg)
			} else {
				slog.Info("platform override loaded", "path", override)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	root := opts.Root
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		root = abs
	}
	return &Platform{Config: cfg, Root: root, DataDir: opts.DataDir, Dev: opts.Dev, SetAside: setAside}, nil
}

// CardPath returns the host path of a card (with the dev root applied).
func (p *Platform) CardPath(c Card) string {
	if p.Root == "" {
		return c.Path
	}
	return filepath.Join(p.Root, filepath.FromSlash(c.Path))
}

// AvailableCards returns the cards that are present. On the device a card
// counts as present if its path is a mount point or already holds the Roms
// folder, so an empty /mnt/sdcard without TF2 inserted is skipped.
func (p *Platform) AvailableCards() []Card {
	var out []Card
	for _, c := range p.Cards {
		hostPath := p.CardPath(c)
		st, err := os.Stat(hostPath)
		if err != nil || !st.IsDir() {
			continue
		}
		if !p.Dev && p.Root == "" && !isMountPoint(hostPath) {
			if fi, err := os.Stat(filepath.Join(hostPath, p.RomsDir)); err != nil || !fi.IsDir() {
				continue
			}
		}
		c.Path = hostPath
		out = append(out, c)
	}
	return out
}

// RomsPath is <card>/Roms.
func (p *Platform) RomsPath(c Card) string {
	return filepath.Join(c.Path, p.RomsDir)
}

// SystemByDir finds a known system by its folder name (case-insensitive,
// aliases included).
func (p *Platform) SystemByDir(dir string) (System, bool) {
	d := strings.ToLower(strings.TrimSpace(dir))
	for _, s := range p.Systems {
		if strings.ToLower(s.Dir) == d {
			return s, true
		}
		for _, a := range s.Aliases {
			if a == d {
				return s, true
			}
		}
	}
	return System{}, false
}

// KeepsArchives reports whether a Roms-relative folder ("Roms/ARCADE")
// belongs to a system whose ROMs stay zipped.
func (p *Platform) KeepsArchives(rel string) bool {
	if s, ok := p.SystemByDir(path.Base(rel)); ok {
		return s.KeepArchives
	}
	return false
}

// archiveSuffix is stripped before looking at the "real" extension, so
// "Game (USA).nds.7z" and "Game.nds.part1.rar" still hint at NDS.
var archiveSuffix = regexp.MustCompile(`(\.zip|\.7z(\.\d{3})?|(\.part\d+)?\.rar)$`)

// SuggestSystem guesses the target system for a remote file: first by file
// extension (looking through an archive suffix), then by any folder name in
// the remote path that matches a system folder or alias.
func (p *Platform) SuggestSystem(fileName, remoteDir string) (System, bool) {
	name := archiveSuffix.ReplaceAllString(strings.ToLower(fileName), "")
	if ext := path.Ext(name); ext != "" {
		for _, s := range p.Systems {
			for _, e := range s.Extensions {
				if e == ext {
					return s, true
				}
			}
		}
	}
	// Walk the remote path from the deepest folder up.
	parts := strings.Split(strings.Trim(path.Clean("/"+remoteDir), "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "" {
			continue
		}
		if s, ok := p.SystemByDir(parts[i]); ok {
			return s, true
		}
	}
	return System{}, false
}

// ExistingDirFor returns the folder name to use for a system on a card:
// an existing folder that matches the system (any case / alias) wins over
// the canonical name, so we never create "NDS" next to an existing "nds".
func (p *Platform) ExistingDirFor(c Card, s System) string {
	entries, err := os.ReadDir(p.RomsPath(c))
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if got, ok := p.SystemByDir(e.Name()); ok && got.Dir == s.Dir {
				return e.Name()
			}
		}
	}
	return s.Dir
}

// RomDirs lists the folder names under Roms on the given cards (existing
// folders first, merged case-insensitively), sorted.
func (p *Platform) RomDirs(cards []Card) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cards {
		entries, err := os.ReadDir(p.RomsPath(c))
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") {
				continue
			}
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// HasNetwork reports whether any non-loopback interface is up with an
// address. It is a cheap "is Wi-Fi on" check before connecting.
func HasNetwork() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true // unknown: let the connection attempt decide
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.IsGlobalUnicast() {
				return true
			}
		}
	}
	return false
}

// RequestRescan leaves a flag for the launcher script, which refreshes the
// frontend's game list after the app exits.
func (p *Platform) RequestRescan() {
	if p.DataDir == "" {
		return
	}
	f := filepath.Join(p.DataDir, ".rescan")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		slog.Warn("rescan flag", "err", err)
	}
}

// ScreensSwapped reports whether the system settings put the main screen on
// display 1.
func (p *Platform) ScreensSwapped() bool {
	data, err := os.ReadFile(p.fsPath(p.ScreenSwapFile))
	return err == nil && strings.TrimSpace(string(data)) == "1"
}

// fsPath maps a device path from the config to the filesystem ("" stays
// "", which no read finds).
func (p *Platform) fsPath(devicePath string) string {
	if devicePath == "" || p.Root == "" {
		return filepath.FromSlash(devicePath)
	}
	return filepath.Join(p.Root, filepath.FromSlash(devicePath))
}

// DeviceID returns the console's unique ID, nil when there is none (a
// computer in dev mode) or it reads as all zeros.
func (p *Platform) DeviceID() []byte {
	if p.DeviceIDFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.fsPath(p.DeviceIDFile))
	if err != nil {
		return nil
	}
	id := strings.Trim(string(data), "\x00 \t\r\n")
	if strings.Trim(id, "0") == "" {
		return nil
	}
	return []byte(id)
}

// FreeSpace returns a card's free space for display. In dev mode
// DSFETCH_FAKE_FREE overrides it (screenshots); space checks before writing
// use the real filesystem (CheckSpace).
func (p *Platform) FreeSpace(c Card) (uint64, bool) {
	if p.Dev {
		if v, ok := fakeFree(c.ID); ok {
			return v, true
		}
	}
	fi, err := Stat(c.Path)
	if err != nil {
		return 0, false
	}
	return fi.Free, true
}

// SystemLanguage returns the language chosen in the device's own settings
// ("ru", "en", ...), or "" when the platform does not tell.
func (p *Platform) SystemLanguage() string {
	data, err := os.ReadFile(p.fsPath(p.LanguageFile))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	i, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || i < 0 || i >= len(p.LanguageCodes) {
		return ""
	}
	return p.LanguageCodes[i]
}

// InhibitSleep keeps the console awake until the returned release is
// called: take it around work (a transfer, unpacking), not around waiting
// for the user. Calls nest: the inhibit command runs on the first call and
// the allow command after the last release; the sleep watcher does not
// suspend while any hold is active, and its idle timer starts over when the
// last hold is released.
func (p *Platform) InhibitSleep() (release func()) {
	p.sleepMu.Lock()
	p.sleepHolds++
	if p.sleepHolds == 1 && p.InhibitSleepCmd != "" {
		runShell(p.InhibitSleepCmd)
	}
	p.sleepMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.sleepMu.Lock()
			defer p.sleepMu.Unlock()
			p.sleepHolds--
			if p.sleepHolds == 0 {
				p.Touch()
				if p.AllowSleepCmd != "" {
					runShell(p.AllowSleepCmd)
				}
			}
		})
	}
}

func runShell(cmd string) {
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		slog.Warn("platform command failed", "cmd", cmd, "err", err, "out", string(out))
	}
}
