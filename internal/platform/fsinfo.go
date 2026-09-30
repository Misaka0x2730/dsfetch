package platform

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// FAT32MaxFileSize is the largest file FAT32 can store (4 GiB - 1).
const FAT32MaxFileSize = 1<<32 - 1

// FSInfo describes the filesystem holding a path.
type FSInfo struct {
	Free uint64 // bytes available to unprivileged users
	Type string // "vfat", "exfat", "ext4", "apfs", ... ("" if unknown)
}

// SameFilesystem reports whether two existing paths are on one filesystem,
// so that a rename can move files between them.
func SameFilesystem(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	da, okA := sa.Sys().(*syscall.Stat_t)
	db, okB := sb.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return true, nil // no device numbers here: let the rename decide
	}
	return da.Dev == db.Dev, nil
}

// IsFAT32 reports whether files are limited to 4 GiB.
func (i FSInfo) IsFAT32() bool {
	return i.Type == "vfat" || i.Type == "msdos"
}

// fakeFree returns the free space DSFETCH_FAKE_FREE sets for a card
// ("tf1=2.78G,tf2=25.2G"; K, M, G and T count in 1024s), so screenshots made
// on a computer show card-sized numbers.
func fakeFree(cardID string) (uint64, bool) {
	for _, kv := range strings.Split(os.Getenv("DSFETCH_FAKE_FREE"), ",") {
		id, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && id == cardID {
			return parseSize(val)
		}
	}
	return 0, false
}

// parseSize reads "2.78G", "512M", "25.2GB" or plain bytes.
func parseSize(s string) (uint64, bool) {
	s = strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), "B")
	mult := 1.0
	for i, unit := range []string{"K", "M", "G", "T"} {
		if strings.HasSuffix(s, unit) {
			s, mult = strings.TrimSuffix(s, unit), math.Pow(1024, float64(i+1))
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return uint64(v * mult), true
}

// Stat returns filesystem information for the nearest existing ancestor of
// path (the target folder may not exist yet).
func Stat(path string) (FSInfo, error) {
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return statfs(p)
}

// devOverrides lets the DSFETCH_FAKE_MEMAVAILABLE development aid work; off
// in normal use, so a stray variable cannot weaken the memory checks.
var devOverrides atomic.Bool

// SetDevOverrides turns the development aids of this package on (-dev).
func SetDevOverrides(on bool) { devOverrides.Store(on) }

// MemAvailable returns MemAvailable from /proc/meminfo in bytes. With
// SetDevOverrides, the env var DSFETCH_FAKE_MEMAVAILABLE (bytes) overrides
// it, which lets the low-memory warning be exercised on a Mac.
func MemAvailable() (uint64, bool) {
	if v := os.Getenv("DSFETCH_FAKE_MEMAVAILABLE"); v != "" && devOverrides.Load() {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n, true
		}
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return kb * 1024, true
			}
		}
	}
	return 0, false
}

// NoSpaceError means the target filesystem is too full.
type NoSpaceError struct{ Need, Free int64 }

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("not enough free space: need %d bytes, %d available", e.Need, e.Free)
}

// FAT32LimitError means a file is too big for a FAT32 card.
type FAT32LimitError struct{ Size int64 }

func (e *FAT32LimitError) Error() string {
	return fmt.Sprintf("a file of %d bytes exceeds the FAT32 limit of 4 GiB", e.Size)
}

// CheckSpace verifies that need bytes fit on the filesystem holding dir and
// that no single file (largest) breaks the FAT32 limit. If the filesystem
// cannot be queried the check passes; the write itself will fail if it must.
func CheckSpace(dir string, need, largest int64) error {
	info, err := Stat(dir)
	if err != nil {
		return nil
	}
	if largest > FAT32MaxFileSize && info.IsFAT32() {
		return &FAT32LimitError{Size: largest}
	}
	if need > 0 && int64(info.Free) < need {
		return &NoSpaceError{Need: need, Free: int64(info.Free)}
	}
	return nil
}
