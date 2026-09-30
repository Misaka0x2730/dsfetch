package archive

import (
	"context"
	"errors"
	"io/fs"

	"github.com/bodgit/sevenzip"
	"go.uber.org/atomic"
)

// SevenZip extracts .7z with the pure Go decoder (LZMA, LZMA2, BCJ/BCJ2,
// PPMd, AES, multi-volume). Its speed on the console's Cortex-A55 is the main
// risk; SevenZipCLI is the drop-in alternative.
type SevenZip struct {
	Options
}

// List implements Extractor.
func (s *SevenZip) List(path string) ([]string, int64, error) {
	info, err := inspect7z(path, s.Password)
	return info.Files, info.UnpackedSize, err
}

// inspect7z lists the archive, returning ErrPasswordRequired when the
// archive is encrypted and no password was given.
func inspect7z(path, password string) (Info, error) {
	hdr, herr := ReadSevenZipHeader(path)
	if herr == nil && hdr.HeaderEncrypted && password == "" {
		return Info{}, ErrPasswordRequired
	}
	rc, err := sevenzip.OpenReaderWithPassword(path, password)
	if err != nil {
		if herr == nil && hdr.HeaderEncrypted {
			return Info{}, ErrWrongPassword
		}
		return Info{}, err
	}
	defer rc.Close()
	entries, total, largest, err := planEntries(sevenZipEntries(rc))
	if err != nil {
		return Info{}, err
	}
	info := Info{Files: fileNames(entries), UnpackedSize: total, LargestFile: largest, renamed: anyRenamed(entries)}
	if herr == nil && hdr.DataEncrypted && password == "" && total > 0 {
		return info, ErrPasswordRequired
	}
	return info, nil
}

func sevenZipEntries(rc *sevenzip.ReadCloser) []rawEntry {
	raw := make([]rawEntry, 0, len(rc.File))
	for _, f := range rc.File {
		f := f
		fi := f.FileInfo()
		raw = append(raw, rawEntry{
			name:    f.Name,
			isDir:   fi.IsDir(),
			symlink: fi.Mode()&fs.ModeSymlink != 0,
			size:    int64(f.UncompressedSize),
			mtime:   f.Modified,
			open:    f.Open,
		})
	}
	return raw
}

// Extract implements Extractor. Files are read in archive order, which keeps
// solid blocks streaming instead of re-decoding from the start.
func (s *SevenZip) Extract(ctx context.Context, path, dst string, progress *atomic.Float64) error {
	if _, err := inspect7z(path, s.Password); err != nil {
		return err
	}
	rc, err := sevenzip.OpenReaderWithPassword(path, s.Password)
	if err != nil {
		return err
	}
	defer rc.Close()
	entries, total, largest, err := planEntries(sevenZipEntries(rc))
	if err != nil {
		return err
	}
	err = extractEntries(ctx, entries, total, largest, dst, s.Options, progress)
	var re *sevenzip.ReadError
	if errors.As(err, &re) && re.Encrypted {
		if s.Password == "" {
			return ErrPasswordRequired
		}
		return ErrWrongPassword
	}
	return err
}
