package archive

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/atomic"

	"dsfetch/internal/platform"
)

const copyBufferSize = 256 << 10

// rawEntry is a member as the format reader reports it.
type rawEntry struct {
	name    string // already decoded to UTF-8
	isDir   bool
	symlink bool
	size    int64
	mtime   time.Time
	open    func() (io.ReadCloser, error)
}

// entry is a validated member with a safe relative path.
type entry struct {
	rawEntry
	rel     string
	renamed bool // rel differs from the name in the archive (FAT rules)
}

// planEntries validates every name (rejecting the archive on zip-slip),
// drops junk and symlinks, makes the names valid on the FAT cards, and
// totals the sizes.
func planEntries(raw []rawEntry) (entries []entry, total, largest int64, err error) {
	for _, r := range raw {
		rel, err := cleanEntryName(r.name)
		if err != nil {
			return nil, 0, 0, err
		}
		if rel == "" || isJunk(rel) || r.symlink {
			continue
		}
		safe := fatPath(rel)
		entries = append(entries, entry{rawEntry: r, rel: safe, renamed: safe != rel})
		if !r.isDir {
			total += r.size
			if r.size > largest {
				largest = r.size
			}
		}
	}
	// A renamed file must not land on another member ("a:b" and "a_b"):
	// the names that needed no change go first. FAT ignores case.
	taken := map[string]bool{}
	for _, e := range entries {
		if !e.renamed || e.isDir {
			taken[strings.ToLower(e.rel)] = true
		}
	}
	for i, e := range entries {
		if e.renamed && !e.isDir {
			entries[i].rel = unusedName(e.rel, taken)
			taken[strings.ToLower(entries[i].rel)] = true
		}
	}
	return entries, total, largest, nil
}

// fatPath makes every part of a slash path valid on FAT32/exFAT.
func fatPath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = platform.SafeFileName(p)
	}
	return strings.Join(parts, "/")
}

// unusedName returns p, or "name (2).ext", "name (3).ext", ... when p is
// taken.
func unusedName(p string, taken map[string]bool) string {
	if !taken[strings.ToLower(p)] {
		return p
	}
	ext := path.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s (%d)%s", base, n, ext)
		if !taken[strings.ToLower(c)] {
			return c
		}
	}
}

// anyRenamed reports whether some member gets another name on the card.
func anyRenamed(entries []entry) bool {
	for _, e := range entries {
		if e.renamed {
			return true
		}
	}
	return false
}

func fileNames(entries []entry) []string {
	var out []string
	for _, e := range entries {
		if !e.isDir {
			out = append(out, e.rel)
		}
	}
	return out
}

// extractEntries is the shared pipeline: space check, unpack into a temp
// folder on dst's card, optional flattening, then move into dst.
func extractEntries(ctx context.Context, entries []entry, total, largest int64, dst string, opts Options, progress *atomic.Float64) error {
	if progress == nil {
		progress = atomic.NewFloat64(0)
	}
	progress.Store(0)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := platform.CheckSpace(dst, total+(1<<20), largest); err != nil {
		return err
	}
	tmp, cleanup, err := tempDirFor(dst, opts.TempRoot)
	if err != nil {
		return err
	}
	defer cleanup()

	buf := make([]byte, copyBufferSize)
	var written int64
	report := func(n int64) {
		written += n
		if total > 0 {
			progress.Store(float64(written) / float64(total))
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(tmp, filepath.FromSlash(e.rel))
		if !within(tmp, target) {
			return &UnsafePathError{Name: e.name}
		}
		if e.isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeEntry(ctx, e, target, buf, report); err != nil {
			return err
		}
	}

	root, err := contentRoot(tmp, opts.FlattenSingleDir)
	if err != nil {
		return err
	}
	if _, err := moveInto(root, dst); err != nil {
		return err
	}
	progress.Store(1)
	return nil
}

func writeEntry(ctx context.Context, e entry, target string, buf []byte, report func(int64)) error {
	rc, err := e.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return err
		}
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			report(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !e.mtime.IsZero() {
		_ = os.Chtimes(target, e.mtime, e.mtime)
	}
	return nil
}

// removeJunkAndLinks cleans a folder unpacked by an external tool.
func removeJunkAndLinks(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.Type()&fs.ModeSymlink != 0 || isJunk(filepath.ToSlash(rel)) {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
}
