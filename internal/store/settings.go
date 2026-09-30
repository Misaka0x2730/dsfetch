package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Choice values for ask/always/never settings.
const (
	Ask    = "ask"
	Always = "always"
	Never  = "never"
)

// 7z engines.
const (
	SevenZipAuto    = "auto"    // 7zz when bundled and working, else built-in
	SevenZipBuiltin = "builtin" // pure Go (bodgit/sevenzip)
	SevenZipCLI     = "7zz"     // external 7zz binary in the app folder
)

// Settings are user preferences (data/settings.json).
type Settings struct {
	Language         string `json:"language"`           // "auto" or a code from i18n.Languages
	ExtractMode      string `json:"extract_mode"`       // unpack after download: Ask/Always/Never
	DeleteArchive    string `json:"delete_archive"`     // delete the archive after unpacking: Ask/Always/Never
	FlattenSingleDir bool   `json:"flatten_single_dir"` // unwrap archives that contain a single folder
	ShowHidden       bool   `json:"show_hidden"`        // show dot-files on servers
	SevenZipEngine   string `json:"sevenzip_engine"`    // SevenZipAuto, SevenZipBuiltin or SevenZipCLI
	LidSleep         bool   `json:"lid_sleep"`          // suspend when the lid is closed and nothing runs
	IdleSleep        bool   `json:"idle_sleep"`         // suspend after the system sleep timer when nothing runs
}

// DefaultSettings are used for a fresh install and for missing fields.
func DefaultSettings() Settings {
	return Settings{
		Language:         "auto",
		ExtractMode:      Ask,
		DeleteArchive:    Ask,
		FlattenSingleDir: true,
		// 7zz single-threaded was ~5x faster than the pure Go decoder with
		// less memory in the Mac benchmark; confirm with -bench on the device.
		SevenZipEngine: SevenZipAuto,
		LidSleep:       true,
		IdleSleep:      true,
	}
}

// LoadSettings reads <dir>/settings.json, falling back to defaults.
func LoadSettings(dir string) (Settings, error) {
	s := DefaultSettings()
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings(), err
	}
	return s, nil
}

// SaveSettings writes <dir>/settings.json.
func SaveSettings(dir string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "settings.json"), append(data, '\n'), 0o644)
}
