package archive

import (
	"archive/zip"
	"compress/bzip2"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"go.uber.org/atomic"
	"golang.org/x/text/encoding/charmap"
)

// Zip extracts .zip archives with the standard library, adding:
//   - names from archivers that do not use UTF-8 (Windows' built-in zip
//     writes the OEM code page: CP866 on Russian systems, CP437 elsewhere);
//   - bzip2 (12) and zstd (93) compression besides store/deflate;
//   - a clear error for encrypted entries, which archive/zip cannot read.
type Zip struct {
	Options
}

const (
	zipMethodBzip2 = 12
	zipMethodZstd  = 93
	flagEncrypted  = 0x1
	flagUTF8       = 0x800
)

func openZip(path string) (*zip.ReadCloser, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	r.RegisterDecompressor(zipMethodBzip2, func(in io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(in))
	})
	r.RegisterDecompressor(zipMethodZstd, func(in io.Reader) io.ReadCloser {
		d, err := zstd.NewReader(in, zstd.WithDecoderLowmem(true), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return io.NopCloser(errReader{err})
		}
		return d.IOReadCloser()
	})
	return r, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func zipEntries(r *zip.ReadCloser) ([]rawEntry, error) {
	guess := guessZipCharset(r.File)
	raw := make([]rawEntry, 0, len(r.File))
	for _, f := range r.File {
		if f.Flags&flagEncrypted != 0 && !f.FileInfo().IsDir() {
			return nil, ErrEncryptedZip
		}
		switch f.Method {
		case zip.Store, zip.Deflate, zipMethodBzip2, zipMethodZstd:
		default:
			if !f.FileInfo().IsDir() {
				return nil, &UnsupportedMethodError{Name: f.Name, Method: f.Method}
			}
		}
		f := f
		mode := f.Mode()
		raw = append(raw, rawEntry{
			name:    zipName(f, guess),
			isDir:   mode.IsDir(),
			symlink: mode&fs.ModeSymlink != 0,
			size:    int64(f.UncompressedSize64),
			mtime:   f.Modified,
			open:    f.Open,
		})
	}
	return raw, nil
}

// List implements Extractor.
func (z *Zip) List(path string) ([]string, int64, error) {
	info, err := inspectZip(path)
	return info.Files, info.UnpackedSize, err
}

func inspectZip(path string) (Info, error) {
	r, err := openZip(path)
	if err != nil {
		return Info{}, err
	}
	defer r.Close()
	raw, err := zipEntries(r)
	if err != nil {
		return Info{}, err
	}
	entries, total, largest, err := planEntries(raw)
	if err != nil {
		return Info{}, err
	}
	return Info{Files: fileNames(entries), UnpackedSize: total, LargestFile: largest}, nil
}

// Extract implements Extractor.
func (z *Zip) Extract(ctx context.Context, path, dst string, progress *atomic.Float64) error {
	r, err := openZip(path)
	if err != nil {
		return err
	}
	defer r.Close()
	raw, err := zipEntries(r)
	if err != nil {
		return err
	}
	entries, total, largest, err := planEntries(raw)
	if err != nil {
		return err
	}
	err = extractEntries(ctx, entries, total, largest, dst, z.Options, progress)
	if errors.Is(err, zip.ErrAlgorithm) {
		return &UnsupportedMethodError{Name: path}
	}
	return err
}

// zipName decodes an entry name: UTF-8 flag, then the Info-ZIP Unicode Path
// extra field, then valid UTF-8 as-is, then the archive's guessed OEM code
// page.
func zipName(f *zip.File, guess *charmap.Charmap) string {
	name := f.Name
	if f.Flags&flagUTF8 != 0 || !hasHighBytes(name) {
		return name
	}
	if u, ok := unicodePathExtra(f.Extra, name); ok {
		return u
	}
	if utf8.ValidString(name) {
		return name // UTF-8 without the flag (common from Linux tools)
	}
	if s, err := guess.NewDecoder().String(name); err == nil {
		return s
	}
	return name
}

func hasHighBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// unicodePathExtra reads the Info-ZIP Unicode Path field (0x7075): version,
// CRC32 of the header name, then the UTF-8 name. The CRC guards against a
// stale field after the entry was renamed by a tool that ignored it.
func unicodePathExtra(extra []byte, rawName string) (string, bool) {
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[0:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		if 4+size > len(extra) {
			return "", false
		}
		body := extra[4 : 4+size]
		if tag == 0x7075 && len(body) >= 5 && body[0] == 1 {
			if binary.LittleEndian.Uint32(body[1:5]) == crc32.ChecksumIEEE([]byte(rawName)) {
				if u := string(body[5:]); utf8.ValidString(u) {
					return u, true
				}
			}
		}
		extra = extra[4+size:]
	}
	return "", false
}

// guessZipCharset picks CP866 when every non-ASCII byte of the non-UTF-8
// names falls into CP866's Cyrillic letter ranges, else CP437 (the zip
// default). CP437 accented letters overlap those ranges, so an archive of
// pure French names could be misread; Cyrillic is by far the likelier case
// for this app's users.
func guessZipCharset(files []*zip.File) *charmap.Charmap {
	seen := false
	for _, f := range files {
		if f.Flags&flagUTF8 != 0 || utf8.ValidString(f.Name) {
			continue
		}
		for i := 0; i < len(f.Name); i++ {
			b := f.Name[i]
			if b < 0x80 {
				continue
			}
			seen = true
			if !(b <= 0xAF || (b >= 0xE0 && b <= 0xF1)) {
				return charmap.CodePage437
			}
		}
	}
	if seen {
		return charmap.CodePage866
	}
	return charmap.CodePage437
}
