package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/nwaples/rardecode/v2"
	"go.uber.org/atomic"

	"dsfetch/internal/platform"
)

// Rar extracts .rar (RAR 1.5 to 7, solid, multi-volume, encrypted) with the
// pure Go decoder. SevenZipCLI handles .rar too when 7zz is used.
type Rar struct {
	Options
}

// A RAR5 dictionary can be up to 64 GB and the Go runtime dies on a failed
// allocation, so a dictionary larger than free memory (less rarReserve) is
// refused before it is allocated. rarMinDict always passes: RAR 1.5-4 need
// at most 4 MB.
const (
	rarReserve = 64 << 20
	rarMinDict = 16 << 20
)

func rarOptions(password string) []rardecode.Option {
	opts := []rardecode.Option{rardecode.BufferSize(copyBufferSize)}
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}
	if avail, ok := platform.MemAvailable(); ok {
		opts = append(opts, rardecode.MaxDictionarySize(max(int64(avail)-rarReserve, rarMinDict)))
	}
	return opts
}

// rarError maps the decoder's errors to the package's.
func rarError(err error, password string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rardecode.ErrArchiveEncrypted), errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		return ErrPasswordRequired
	case errors.Is(err, rardecode.ErrBadPassword):
		if password == "" {
			return ErrPasswordRequired
		}
		return ErrWrongPassword
	case password != "" && (errors.Is(err, rardecode.ErrBadFileChecksum) || errors.Is(err, rardecode.ErrBadHeaderCRC)):
		// RAR 4 archives have no password check value: a wrong password
		// shows up as garbage failing the checksum.
		return ErrWrongPassword
	case errors.Is(err, rardecode.ErrDictionaryTooLarge):
		return ErrOutOfMemory
	}
	return err
}

// List implements Extractor.
func (r *Rar) List(path string) ([]string, int64, error) {
	info, err := inspectRar(path, r.Password)
	return info.Files, info.UnpackedSize, err
}

// inspectRar lists the archive, returning ErrPasswordRequired when files are
// encrypted and no password was given.
func inspectRar(path, password string) (Info, error) {
	files, err := rardecode.List(path, rarOptions(password)...)
	if err != nil {
		return Info{}, rarError(err, password)
	}
	entries, total, largest, err := planEntries(rarEntries(files, nil))
	if err != nil {
		return Info{}, err
	}
	info := Info{Files: fileNames(entries), UnpackedSize: total, LargestFile: largest, renamed: anyRenamed(entries)}
	if password == "" {
		for _, f := range files {
			if f.Encrypted && !f.IsDir {
				return info, ErrPasswordRequired
			}
		}
	}
	return info, nil
}

// rarEntries converts the listing; open, when set, serves entry i's content.
func rarEntries(files []*rardecode.File, open func(i int, name string) (io.ReadCloser, error)) []rawEntry {
	raw := make([]rawEntry, 0, len(files))
	for i, f := range files {
		i, name := i, f.Name
		e := rawEntry{
			name:    name,
			isDir:   f.IsDir,
			symlink: f.LinkType != 0 || f.Mode()&fs.ModeSymlink != 0,
			size:    f.UnPackedSize,
			mtime:   f.ModificationTime,
		}
		if f.UnKnownSize {
			e.size = 0
		}
		if open != nil {
			e.open = func() (io.ReadCloser, error) { return open(i, name) }
		}
		raw = append(raw, e)
	}
	return raw
}

// rarCursor serves entries from the sequential reader. Solid archives can
// only be decoded front to back; extractEntries opens entries in archive
// order, so each open just advances the reader to the requested file.
type rarCursor struct {
	rc   *rardecode.ReadCloser
	next int // index of the file the next Next() returns
}

func (c *rarCursor) open(i int, name string) (io.ReadCloser, error) {
	for c.next <= i {
		h, err := c.rc.Next()
		if err != nil {
			return nil, err
		}
		c.next++
		if c.next-1 == i {
			if h.Name != name {
				break
			}
			return io.NopCloser(c.rc), nil
		}
	}
	return nil, fmt.Errorf("archive: %q is out of order in the RAR listing", name)
}

// Extract implements Extractor.
func (r *Rar) Extract(ctx context.Context, path, dst string, progress *atomic.Float64) error {
	opts := rarOptions(r.Password)
	files, err := rardecode.List(path, opts...)
	if err != nil {
		return rarError(err, r.Password)
	}
	rc, err := rardecode.OpenReader(path, opts...)
	if err != nil {
		return rarError(err, r.Password)
	}
	defer rc.Close()
	cur := &rarCursor{rc: rc}
	entries, total, largest, err := planEntries(rarEntries(files, cur.open))
	if err != nil {
		return err
	}
	err = rarError(extractEntries(ctx, entries, total, largest, dst, r.Options, progress), r.Password)
	if err != nil && r.Password != "" && strings.HasPrefix(err.Error(), "rardecode:") && slices.ContainsFunc(files, func(f *rardecode.File) bool { return f.Encrypted }) {
		// RAR 4 has no password check value: with a wrong password the
		// decoder reads garbage and fails somewhere in the data.
		return ErrWrongPassword
	}
	return err
}

var (
	rar5Signature = []byte("Rar!\x1a\x07\x01\x00")
	rar4Signature = []byte("Rar!\x1a\x07\x00")
)

// RarDecoderMemory estimates the RAM the RAR decoder needs: the largest
// dictionary among the files, as the decoder allocates it (not more than the
// file's size unless the file is solid). Only headers are read; data is
// skipped with seeks. RAR 1.5-4 dictionaries are at most 4 MB and archives
// with encrypted headers cannot be read without the password; both report 0.
func RarDecoderMemory(path string) (int64, error) {
	var largest int64
	for _, vol := range Volumes(path) {
		w, err := rarVolumeWindow(vol)
		if err != nil {
			return largest, err
		}
		largest = max(largest, w)
	}
	return largest, nil
}

func rarVolumeWindow(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sig := make([]byte, len(rar5Signature))
	if _, err := io.ReadFull(f, sig); err != nil {
		return 0, err
	}
	if !bytes.Equal(sig, rar5Signature) {
		if bytes.HasPrefix(sig, rar4Signature) {
			return 0, nil
		}
		return 0, rardecode.ErrNoSig
	}
	var largest int64
	pos := int64(len(rar5Signature))
	for n := 0; n < 1<<16; n++ {
		h, next, err := readRar5Block(f, pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return largest, nil
			}
			return largest, err
		}
		switch h.kind {
		case rar5BlockFile:
			largest = max(largest, h.window())
		case rar5BlockEncrypt, rar5BlockEnd:
			return largest, nil
		}
		pos = next
	}
	return largest, nil
}

const (
	rar5BlockFile    = 2
	rar5BlockEncrypt = 4
	rar5BlockEnd     = 5

	rar5HasExtra        = 0x1
	rar5HasData         = 0x2
	rar5DataNotFirst    = 0x8
	rar5FileIsDir       = 0x1
	rar5FileHasMtime    = 0x2
	rar5FileHasCRC      = 0x4
	rar5FileSizeUnknown = 0x8
	rar5CompSolid       = 0x40
	rar5CompMethod      = 0x380
	rar5CompDictSize    = 0x7c00
	rar5CompDictFract   = 0xf8000
	rar5MaxHeaderSize   = 2 << 20
)

type rar5Block struct {
	kind     uint64
	dataSize uint64
	// file blocks only
	isDir, solid, compressed, firstPart, sizeUnknown bool
	unpacked, dict                                   int64
}

// window is the dictionary the decoder allocates for a file block.
func (b rar5Block) window() int64 {
	if b.isDir || !b.compressed || !b.firstPart {
		return 0
	}
	if !b.solid && !b.sizeUnknown && b.dict > b.unpacked {
		return b.unpacked
	}
	return b.dict
}

// readRar5Block reads the block header at pos (CRC32, size, type, flags,
// sizes and, for files, the fields up to the compression info) and returns
// the position of the next block.
func readRar5Block(r io.ReaderAt, pos int64) (rar5Block, int64, error) {
	var b rar5Block
	head := make([]byte, 4+binary.MaxVarintLen64)
	n, err := r.ReadAt(head, pos)
	if n < 5 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return b, 0, err
	}
	size, sn := binary.Uvarint(head[4:n])
	if sn <= 0 || size == 0 || size > rar5MaxHeaderSize {
		return b, 0, rardecode.ErrBadBlockHeader
	}
	start := pos + 4 + int64(sn)
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, start); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return b, 0, err
	}
	hr := bytes.NewReader(buf)
	uv := func() uint64 {
		v, err := binary.ReadUvarint(hr)
		if err != nil {
			return 0
		}
		return v
	}
	b.kind = uv()
	flags := uv()
	if flags&rar5HasExtra != 0 {
		uv()
	}
	if flags&rar5HasData != 0 {
		b.dataSize = uv()
	}
	next := start + int64(size) + int64(b.dataSize)
	if b.kind != rar5BlockFile {
		return b, next, nil
	}
	b.firstPart = flags&rar5DataNotFirst == 0
	fileFlags := uv()
	b.isDir = fileFlags&rar5FileIsDir != 0
	b.sizeUnknown = fileFlags&rar5FileSizeUnknown != 0
	b.unpacked = int64(uv())
	uv() // attributes
	if fileFlags&rar5FileHasMtime != 0 {
		_, _ = hr.Seek(4, io.SeekCurrent)
	}
	if fileFlags&rar5FileHasCRC != 0 {
		_, _ = hr.Seek(4, io.SeekCurrent)
	}
	comp := uv()
	b.solid = comp&rar5CompSolid != 0
	b.compressed = comp&rar5CompMethod != 0
	b.dict = int64(0x20000) << ((comp & rar5CompDictSize) >> 10)
	if comp&0x3f == 1 { // RAR 7 algorithm: fractional dictionary sizes
		b.dict += b.dict / 32 * int64((comp&rar5CompDictFract)>>15)
	}
	return b, next, nil
}
