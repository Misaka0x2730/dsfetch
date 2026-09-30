// Package archive unpacks .zip, .7z and .rar files onto the memory card.
//
// Extraction goes into a temporary folder on the target's card
// (<card>/.dsfetch-tmp) and is moved into place only when complete, so a
// cancelled or failed extraction never leaves half a game in the ROM folder.
// Paths are checked against zip-slip, names are made valid on FAT, macOS and
// Windows junk is skipped, and an archive that holds a single folder can be
// flattened.
//
// Parts of this package are adapted from Grout's fileutil
// (github.com/rommapp/grout, MIT, Copyright (c) 2025 Brandon T. Kowalski,
// Grout Contributors).
package archive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"go.uber.org/atomic"
)

// Extractor unpacks one archive format.
type Extractor interface {
	// List returns the file names inside the archive (junk excluded) and
	// their total unpacked size.
	List(path string) (files []string, unpackedSize int64, err error)
	// Extract unpacks into dst, reporting progress 0..1.
	Extract(ctx context.Context, path, dst string, progress *atomic.Float64) error
}

// Options shared by all extractors.
type Options struct {
	// Password for encrypted 7z and RAR archives.
	Password string
	// FlattenSingleDir unwraps an archive whose content is a single folder.
	FlattenSingleDir bool
	// SevenZipBinary, if set, is the path of a 7zz executable used for .7z
	// and .rar instead of the pure Go decoders.
	SevenZipBinary string
	// TempRoot holds the temporary folder of an unpack in progress: the
	// root of the card with the target folder, so the finished files move
	// into place by rename. Empty means the target folder itself.
	TempRoot string
}

var (
	ErrUnsupported      = errors.New("archive: not a .zip, .7z or .rar file")
	ErrEncryptedZip     = errors.New("archive: encrypted zip archives are not supported")
	ErrPasswordRequired = errors.New("archive: the archive is password protected")
	ErrWrongPassword    = errors.New("archive: wrong password")
	// ErrOutOfMemory: the decoder needs more RAM than is free (a large 7z or
	// RAR dictionary).
	ErrOutOfMemory = errors.New("archive: not enough memory to unpack")
)

// UnsafePathError is returned for entries that would be written outside the
// target folder (zip-slip); the whole archive is rejected.
type UnsafePathError struct{ Name string }

func (e *UnsafePathError) Error() string {
	return fmt.Sprintf("archive: unsafe path in archive: %q", e.Name)
}

// UnsupportedMethodError is returned for compression methods we cannot
// decode (for example Deflate64 zips written by Windows Explorer).
type UnsupportedMethodError struct {
	Name   string
	Method uint16
}

func (e *UnsupportedMethodError) Error() string {
	return fmt.Sprintf("archive: %s uses unsupported compression method %d", e.Name, e.Method)
}

// Kind returns "zip", "7z", "rar" or "" from a file name. Multi-volume
// archives are recognised by their first volume only (.7z.001, .part1.rar;
// the old RAR naming starts with .rar and goes on with .r00, .r01, ...).
func Kind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".7z"), strings.HasSuffix(n, ".7z.001"):
		return "7z"
	case strings.HasSuffix(n, ".rar"):
		if m := rarPartRe.FindStringSubmatch(n); m != nil && strings.TrimLeft(m[2], "0") != "1" {
			return "" // a later volume
		}
		return "rar"
	}
	return ""
}

// IsArchive reports whether a file name looks like something we can unpack.
func IsArchive(name string) bool { return Kind(name) != "" }

// For returns the extractor for a file.
func For(path string, opts Options) (Extractor, error) {
	switch Kind(filepath.Base(path)) {
	case "zip":
		return &Zip{Options: opts}, nil
	case "7z":
		if opts.SevenZipBinary != "" {
			return &SevenZipCLI{Options: opts}, nil
		}
		return &SevenZip{Options: opts}, nil
	case "rar":
		if opts.SevenZipBinary != "" {
			return &SevenZipCLI{Options: opts}, nil
		}
		return &Rar{Options: opts}, nil
	}
	return nil, ErrUnsupported
}

// Info describes an archive before extraction, for the UI's checks.
type Info struct {
	Files        []string
	UnpackedSize int64
	LargestFile  int64
	// DecoderMemory is the RAM the decompressor needs (7z: LZMA dictionary or
	// PPMd model size; RAR: dictionary), 0 when small or unknown.
	DecoderMemory int64

	renamed bool // some names change to be valid on FAT (7z and RAR only)
}

// Inspect lists an archive and estimates the resources extraction needs.
// It returns ErrPasswordRequired for encrypted 7z and RAR archives opened
// without a password, and ErrEncryptedZip for encrypted zips.
func Inspect(path string, opts Options) (Info, error) {
	switch Kind(filepath.Base(path)) {
	case "zip":
		return inspectZip(path)
	case "7z":
		info, err := inspect7z(path, opts.Password)
		if err != nil {
			return info, err
		}
		if mem, err := SevenZipDecoderMemory(path); err == nil {
			info.DecoderMemory = mem
		}
		return info, nil
	case "rar":
		info, err := inspectRar(path, opts.Password)
		if err != nil {
			return info, err
		}
		if mem, err := RarDecoderMemory(path); err == nil {
			info.DecoderMemory = mem
		}
		return info, nil
	}
	return Info{}, ErrUnsupported
}

var (
	sevenZipVolRe = regexp.MustCompile(`(?i)^(.*\.7z)\.(\d{3})$`)
	rarPartRe     = regexp.MustCompile(`(?i)^(.*)\.part(\d+)\.rar$`)
	rarOldVolRe   = regexp.MustCompile(`(?i)^(.*)\.([rs])(\d{2})$`)
)

// Volumes returns every file that belongs to an archive (all parts of a
// multi-volume 7z or RAR), e.g. to delete them after extraction.
func Volumes(path string) []string {
	dir, name := filepath.Split(path)
	switch {
	case sevenZipVolRe.MatchString(name):
		m := sevenZipVolRe.FindStringSubmatch(name)
		return existing(func(i int) string { return fmt.Sprintf("%s%s.%03d", dir, m[1], i+1) })
	case rarPartRe.MatchString(name):
		m := rarPartRe.FindStringSubmatch(name)
		width := len(m[2])
		return existing(func(i int) string { return fmt.Sprintf("%s%s.part%0*d.rar", dir, m[1], width, i+1) })
	case strings.HasSuffix(strings.ToLower(name), ".rar"):
		// Old naming: game.rar, game.r00 ... game.r99, game.s00 ... game.s99.
		base := path[:len(path)-len(".rar")]
		r := "r"
		if strings.HasSuffix(name, ".RAR") {
			r = "R"
		}
		out := []string{path}
		for i := 0; i < 200; i++ {
			v := fmt.Sprintf("%s.%c%02d", base, rune(r[0])+rune(i/100), i%100)
			if !fileExists(v) {
				break
			}
			out = append(out, v)
		}
		return out
	}
	return []string{path}
}

// existing lists name(0), name(1), ... while the files exist.
func existing(name func(i int) string) []string {
	var out []string
	for i := 0; i < 1000; i++ {
		p := name(i)
		if !fileExists(p) {
			break
		}
		out = append(out, p)
	}
	return out
}

// Volume places a downloaded file in its archive: first is the path of the
// volume to unpack from and key is shared by all volumes of one archive.
// ok is false for files that are not (part of) an archive. A single-file
// archive is its own first volume.
func Volume(path string) (first, key string, ok bool) {
	dir, name := filepath.Split(path)
	if m := sevenZipVolRe.FindStringSubmatch(name); m != nil {
		return dir + m[1] + ".001", strings.ToLower(dir + m[1]), true
	}
	if m := rarPartRe.FindStringSubmatch(name); m != nil {
		first := fmt.Sprintf("%s%s.part%0*d%s", dir, m[1], len(m[2]), 1, name[len(name)-len(".rar"):])
		return first, strings.ToLower(dir+m[1]) + ".part.rar", true
	}
	if m := rarOldVolRe.FindStringSubmatch(name); m != nil {
		ext := ".rar"
		if m[2] == strings.ToUpper(m[2]) {
			ext = ".RAR"
		}
		return dir + m[1] + ext, strings.ToLower(dir+m[1]) + ".rar", true
	}
	if strings.HasSuffix(strings.ToLower(name), ".rar") && Kind(name) == "rar" {
		return path, strings.ToLower(path[:len(path)-len(".rar")]) + ".rar", true
	}
	if Kind(name) != "" {
		return path, strings.ToLower(path), true
	}
	return "", "", false
}
