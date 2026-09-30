package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"dsfetch/internal/platform"
)

// cleanEntryName turns an archive entry name into a safe relative slash
// path. It returns "" for entries to skip (the root itself) and an
// *UnsafePathError for anything that would escape the target folder.
func cleanEntryName(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") || hasDriveLetter(n) {
		return "", &UnsafePathError{Name: name}
	}
	clean := path.Clean(n)
	if clean == "." || clean == "" {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", &UnsafePathError{Name: name}
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", &UnsafePathError{Name: name}
		}
	}
	return clean, nil
}

func hasDriveLetter(p string) bool {
	return len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// isJunk reports files that archivers on macOS and Windows add and that have
// no place in a ROM folder.
func isJunk(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == "__MACOSX" || part == TempDirName {
			return true
		}
	}
	base := path.Base(rel)
	return base == ".DS_Store" || base == "Thumbs.db" || strings.HasPrefix(base, "._")
}

// within verifies that target stays inside root after joining (defence in
// depth on top of cleanEntryName).
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TempDirName is the folder that holds an unpack in progress.
const TempDirName = ".dsfetch-tmp"

// tempDirFor creates <root>/.dsfetch-tmp/<random> for unpacking into dst.
// root is the card root (Options.TempRoot), or dst itself when it is empty:
// on dst's filesystem either way, so the final move is a rename. (Not next
// to dst: for a card root that is the system partition.)
func tempDirFor(dst, root string) (tmp string, cleanup func(), err error) {
	if root == "" {
		root = dst
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", nil, err
	}
	base := filepath.Join(root, TempDirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	tmp, err = os.MkdirTemp(base, "x-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() {
		_ = os.RemoveAll(tmp)
		_ = os.Remove(base) // only succeeds when empty
	}
	// Across filesystems the move would copy everything again (and the
	// space check covered dst only): refuse before writing.
	if same, err := platform.SameFilesystem(tmp, dst); err == nil && !same {
		cleanup()
		return "", nil, fmt.Errorf("archive: %s is not on the same filesystem as %s", base, dst)
	}
	return tmp, cleanup, nil
}

// RemoveStaleTemp deletes what an interrupted unpack (power off, crash) left
// in root's temporary folder, and reports whether there was anything. Call it
// only while nothing is being unpacked.
func RemoveStaleTemp(root string) (bool, error) {
	dir := filepath.Join(root, TempDirName)
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, os.RemoveAll(dir)
}

// contentRoot returns the folder whose children should land in dst: the
// temp folder itself, or its only subfolder when flattening.
func contentRoot(tmp string, flatten bool) (string, error) {
	if !flatten {
		return tmp, nil
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(tmp, entries[0].Name()), nil
	}
	return tmp, nil
}

// moveInto moves every child of src into dst, merging folders and replacing
// files that already exist. It returns the names placed at the top level.
func moveInto(src, dst string) ([]string, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if err := moveMerge(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return names, err
		}
		names = append(names, e.Name())
	}
	return names, nil
}

func moveMerge(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	dstInfo, err := os.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Rename(src, dst)
	case err != nil:
		return err
	case srcInfo.IsDir() && dstInfo.IsDir():
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := moveMerge(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Remove(src)
	default:
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		return os.Rename(src, dst)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
