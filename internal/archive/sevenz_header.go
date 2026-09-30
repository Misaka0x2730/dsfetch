package archive

// A minimal reader for the 7z header, used only to learn which coders the
// archive uses and with which properties. bodgit/sevenzip does not expose
// them, and the LZMA dictionary (or PPMd model) size decides whether
// decompression fits into the console's 1 GB of RAM.
//
// Format reference: 7-Zip's DOC/7zFormat.txt.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ulikunitz/xz/lzma"
)

var (
	errNot7z              = errors.New("archive: not a 7z file")
	errHeaderEncrypted    = errors.New("archive: 7z header is encrypted")
	errUnsupportedHeader  = errors.New("archive: unsupported 7z header encoding")
	sevenZipSignature     = []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}
	coderAES              = []byte{0x06, 0xF1, 0x07, 0x01}
	coderLZMA             = []byte{0x03, 0x01, 0x01}
	coderLZMA2            = []byte{0x21}
	coderPPMD             = []byte{0x03, 0x04, 0x01}
	maxDecodedHeaderBytes = uint64(64 << 20)
)

// Property IDs.
const (
	kEnd                   = 0x00
	kHeader                = 0x01
	kArchiveProperties     = 0x02
	kAdditionalStreamsInfo = 0x03
	kMainStreamsInfo       = 0x04
	kFilesInfo             = 0x05
	kPackInfo              = 0x06
	kUnPackInfo            = 0x07
	kSubStreamsInfo        = 0x08
	kSize                  = 0x09
	kCRC                   = 0x0A
	kFolder                = 0x0B
	kCodersUnPackSize      = 0x0C
	kEncodedHeader         = 0x17
)

type sevenZipCoder struct {
	ID    []byte
	Props []byte
}

type sevenZipFolder struct {
	Coders      []sevenZipCoder
	UnpackSizes []uint64
}

type sevenZipStreams struct {
	PackPos   uint64
	PackSizes []uint64
	Folders   []sevenZipFolder
}

// SevenZipHeader summarises what the header says about the data streams.
type SevenZipHeader struct {
	Folders         []sevenZipFolder
	HeaderEncrypted bool // file names are encrypted too (-mhe=on)
	DataEncrypted   bool
}

// DecoderMemory returns the largest decoder memory any folder needs.
func (h *SevenZipHeader) DecoderMemory() int64 {
	var max int64
	for _, f := range h.Folders {
		for _, c := range f.Coders {
			if m := coderMemory(c); m > max {
				max = m
			}
		}
	}
	return max
}

// SevenZipDecoderMemory parses the archive header and returns the RAM its
// decoders need (0 if unknown, e.g. with encrypted headers).
func SevenZipDecoderMemory(path string) (int64, error) {
	h, err := ReadSevenZipHeader(path)
	if err != nil {
		return 0, err
	}
	return h.DecoderMemory(), nil
}

// coderMemory estimates the memory a single decoder allocates.
func coderMemory(c sevenZipCoder) int64 {
	switch {
	case bytes.Equal(c.ID, coderLZMA) && len(c.Props) >= 5:
		return int64(binary.LittleEndian.Uint32(c.Props[1:5]))
	case bytes.Equal(c.ID, coderLZMA2) && len(c.Props) >= 1:
		return int64(lzma2DictSize(c.Props[0]))
	case bytes.Equal(c.ID, coderPPMD) && len(c.Props) >= 5:
		return int64(binary.LittleEndian.Uint32(c.Props[1:5]))
	}
	return 0
}

func lzma2DictSize(p byte) uint64 {
	if p > 40 {
		return 0
	}
	if p == 40 {
		return 0xFFFFFFFF
	}
	return uint64(2|(p&1)) << (p/2 + 11)
}

// ReadSevenZipHeader parses the header of a .7z (or the volumes of a
// .7z.001) file.
func ReadSevenZipHeader(path string) (*SevenZipHeader, error) {
	ra, size, closeAll, err := openVolumes(path)
	if err != nil {
		return nil, err
	}
	defer closeAll()

	sig := make([]byte, 32)
	if _, err := ra.ReadAt(sig, 0); err != nil {
		return nil, errNot7z
	}
	if !bytes.Equal(sig[:6], sevenZipSignature) {
		return nil, errNot7z
	}
	nextOff := binary.LittleEndian.Uint64(sig[12:20])
	nextSize := binary.LittleEndian.Uint64(sig[20:28])
	if nextSize == 0 {
		return &SevenZipHeader{}, nil // empty archive
	}
	if nextSize > maxDecodedHeaderBytes || 32+nextOff+nextSize > uint64(size) {
		return nil, fmt.Errorf("archive: corrupt 7z header location")
	}
	buf := make([]byte, nextSize)
	if _, err := ra.ReadAt(buf, int64(32+nextOff)); err != nil {
		return nil, err
	}

	out := &SevenZipHeader{}
	for depth := 0; depth < 4; depth++ {
		r := &hdrReader{b: buf}
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch id {
		case kHeader:
			folders, err := parseHeader(r)
			if err != nil {
				return nil, err
			}
			out.Folders = folders
			for _, f := range folders {
				for _, c := range f.Coders {
					if bytes.Equal(c.ID, coderAES) {
						out.DataEncrypted = true
					}
				}
			}
			return out, nil
		case kEncodedHeader:
			si, err := parseStreamsInfo(r)
			if err != nil {
				return nil, err
			}
			decoded, err := decodeHeader(ra, si)
			if errors.Is(err, errHeaderEncrypted) {
				out.HeaderEncrypted = true
				out.DataEncrypted = true
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			buf = decoded
		default:
			return nil, fmt.Errorf("archive: unexpected 7z header id 0x%02x", id)
		}
	}
	return nil, errUnsupportedHeader
}

// decodeHeader unpacks an encoded (compressed) header. 7-Zip compresses
// headers with plain LZMA; anything else is reported as unsupported.
func decodeHeader(ra io.ReaderAt, si *sevenZipStreams) ([]byte, error) {
	if len(si.Folders) != 1 || len(si.PackSizes) < 1 {
		return nil, errUnsupportedHeader
	}
	f := si.Folders[0]
	for _, c := range f.Coders {
		if bytes.Equal(c.ID, coderAES) {
			return nil, errHeaderEncrypted
		}
	}
	if len(f.Coders) != 1 || len(f.UnpackSizes) != 1 {
		return nil, errUnsupportedHeader
	}
	c := f.Coders[0]
	unpack := f.UnpackSizes[0]
	if unpack > maxDecodedHeaderBytes || si.PackSizes[0] > maxDecodedHeaderBytes {
		return nil, errUnsupportedHeader
	}
	packed := make([]byte, si.PackSizes[0])
	if _, err := ra.ReadAt(packed, int64(32+si.PackPos)); err != nil {
		return nil, err
	}
	switch {
	case bytes.Equal(c.ID, coderLZMA) && len(c.Props) == 5:
		// Rebuild the classic .lzma header: props, dictionary, size.
		hdr := make([]byte, 13)
		hdr[0] = c.Props[0]
		dict := binary.LittleEndian.Uint32(c.Props[1:5])
		if dict < lzma.MinDictCap {
			dict = lzma.MinDictCap
		}
		binary.LittleEndian.PutUint32(hdr[1:5], dict)
		binary.LittleEndian.PutUint64(hdr[5:13], unpack)
		lr, err := lzma.NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(packed)))
		if err != nil {
			return nil, err
		}
		out := make([]byte, unpack)
		if _, err := io.ReadFull(lr, out); err != nil {
			return nil, err
		}
		return out, nil
	case bytes.Equal(c.ID, coderLZMA2) && len(c.Props) == 1:
		cfg := lzma.Reader2Config{DictCap: int(lzma2DictSize(c.Props[0]))}
		if cfg.DictCap < lzma.MinDictCap {
			cfg.DictCap = lzma.MinDictCap
		}
		lr, err := cfg.NewReader2(bytes.NewReader(packed))
		if err != nil {
			return nil, err
		}
		out := make([]byte, unpack)
		if _, err := io.ReadFull(lr, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, errUnsupportedHeader
}

func parseHeader(r *hdrReader) ([]sevenZipFolder, error) {
	for {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch id {
		case kArchiveProperties:
			if err := skipArchiveProperties(r); err != nil {
				return nil, err
			}
		case kAdditionalStreamsInfo:
			if _, err := parseStreamsInfo(r); err != nil {
				return nil, err
			}
		case kMainStreamsInfo:
			si, err := parseStreamsInfo(r)
			if err != nil {
				return nil, err
			}
			return si.Folders, nil
		case kFilesInfo, kEnd:
			return nil, nil // no data streams (only empty files/folders)
		default:
			return nil, fmt.Errorf("archive: unexpected 7z property 0x%02x", id)
		}
	}
}

func skipArchiveProperties(r *hdrReader) error {
	for {
		t, err := r.byte()
		if err != nil {
			return err
		}
		if t == kEnd {
			return nil
		}
		n, err := r.number()
		if err != nil {
			return err
		}
		if err := r.skip(n); err != nil {
			return err
		}
	}
}

// parseStreamsInfo reads PackInfo and UnPackInfo; it stops at SubStreamsInfo,
// which only describes how folders split into files.
func parseStreamsInfo(r *hdrReader) (*sevenZipStreams, error) {
	si := &sevenZipStreams{}
	for {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch id {
		case kPackInfo:
			if err := parsePackInfo(r, si); err != nil {
				return nil, err
			}
		case kUnPackInfo:
			if err := parseUnpackInfo(r, si); err != nil {
				return nil, err
			}
		case kSubStreamsInfo, kEnd:
			return si, nil
		default:
			return nil, fmt.Errorf("archive: unexpected 7z streams property 0x%02x", id)
		}
	}
}

func parsePackInfo(r *hdrReader, si *sevenZipStreams) error {
	var err error
	if si.PackPos, err = r.number(); err != nil {
		return err
	}
	n, err := r.number()
	if err != nil {
		return err
	}
	if n > 1<<20 {
		return errUnsupportedHeader
	}
	for {
		id, err := r.byte()
		if err != nil {
			return err
		}
		switch id {
		case kSize:
			si.PackSizes = make([]uint64, n)
			for i := range si.PackSizes {
				if si.PackSizes[i], err = r.number(); err != nil {
					return err
				}
			}
		case kCRC:
			if err := skipDigests(r, n); err != nil {
				return err
			}
		case kEnd:
			return nil
		default:
			return fmt.Errorf("archive: unexpected 7z pack property 0x%02x", id)
		}
	}
}

func parseUnpackInfo(r *hdrReader, si *sevenZipStreams) error {
	id, err := r.byte()
	if err != nil {
		return err
	}
	if id != kFolder {
		return fmt.Errorf("archive: expected 7z folder list, got 0x%02x", id)
	}
	n, err := r.number()
	if err != nil {
		return err
	}
	if n > 1<<20 {
		return errUnsupportedHeader
	}
	external, err := r.byte()
	if err != nil {
		return err
	}
	if external != 0 {
		return errUnsupportedHeader
	}
	outStreams := make([]uint64, n)
	si.Folders = make([]sevenZipFolder, n)
	for i := range si.Folders {
		f, outs, err := parseFolder(r)
		if err != nil {
			return err
		}
		si.Folders[i], outStreams[i] = f, outs
	}
	if id, err = r.byte(); err != nil {
		return err
	}
	if id != kCodersUnPackSize {
		return fmt.Errorf("archive: expected 7z unpack sizes, got 0x%02x", id)
	}
	for i := range si.Folders {
		sizes := make([]uint64, outStreams[i])
		for j := range sizes {
			if sizes[j], err = r.number(); err != nil {
				return err
			}
		}
		si.Folders[i].UnpackSizes = sizes
	}
	for {
		id, err := r.byte()
		if err != nil {
			return err
		}
		switch id {
		case kCRC:
			if err := skipDigests(r, n); err != nil {
				return err
			}
		case kEnd:
			return nil
		default:
			return fmt.Errorf("archive: unexpected 7z unpack property 0x%02x", id)
		}
	}
}

func parseFolder(r *hdrReader) (sevenZipFolder, uint64, error) {
	var f sevenZipFolder
	numCoders, err := r.number()
	if err != nil {
		return f, 0, err
	}
	if numCoders == 0 || numCoders > 64 {
		return f, 0, errUnsupportedHeader
	}
	var inTotal, outTotal uint64
	for i := uint64(0); i < numCoders; i++ {
		flag, err := r.byte()
		if err != nil {
			return f, 0, err
		}
		if flag&0x80 != 0 {
			return f, 0, errUnsupportedHeader // alternative methods, never written
		}
		id, err := r.bytes(uint64(flag & 0x0F))
		if err != nil {
			return f, 0, err
		}
		nIn, nOut := uint64(1), uint64(1)
		if flag&0x10 != 0 {
			if nIn, err = r.number(); err != nil {
				return f, 0, err
			}
			if nOut, err = r.number(); err != nil {
				return f, 0, err
			}
		}
		var props []byte
		if flag&0x20 != 0 {
			size, err := r.number()
			if err != nil {
				return f, 0, err
			}
			if props, err = r.bytes(size); err != nil {
				return f, 0, err
			}
		}
		inTotal += nIn
		outTotal += nOut
		f.Coders = append(f.Coders, sevenZipCoder{ID: id, Props: props})
	}
	if outTotal == 0 || outTotal > 64 || inTotal > 64 {
		return f, 0, errUnsupportedHeader
	}
	bindPairs := outTotal - 1
	for i := uint64(0); i < bindPairs; i++ {
		if _, err := r.number(); err != nil {
			return f, 0, err
		}
		if _, err := r.number(); err != nil {
			return f, 0, err
		}
	}
	if inTotal < bindPairs {
		return f, 0, errUnsupportedHeader
	}
	if packed := inTotal - bindPairs; packed > 1 {
		for i := uint64(0); i < packed; i++ {
			if _, err := r.number(); err != nil {
				return f, 0, err
			}
		}
	}
	return f, outTotal, nil
}

func skipDigests(r *hdrReader, n uint64) error {
	all, err := r.byte()
	if err != nil {
		return err
	}
	defined := n
	if all == 0 {
		bits, err := r.bytes((n + 7) / 8)
		if err != nil {
			return err
		}
		defined = 0
		for i := uint64(0); i < n; i++ {
			if bits[i/8]&(0x80>>(i%8)) != 0 {
				defined++
			}
		}
	}
	return r.skip(defined * 4)
}

// hdrReader is a bounds-checked cursor over a header buffer.
type hdrReader struct {
	b   []byte
	off int
}

var errShortHeader = errors.New("archive: truncated 7z header")

func (r *hdrReader) byte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, errShortHeader
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

func (r *hdrReader) bytes(n uint64) ([]byte, error) {
	if n > uint64(len(r.b)-r.off) {
		return nil, errShortHeader
	}
	out := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return out, nil
}

func (r *hdrReader) skip(n uint64) error {
	_, err := r.bytes(n)
	return err
}

// number reads 7z's variable-length UINT64: the count of leading one bits
// in the first byte says how many little-endian bytes follow.
func (r *hdrReader) number() (uint64, error) {
	first, err := r.byte()
	if err != nil {
		return 0, err
	}
	mask := byte(0x80)
	var value uint64
	for i := 0; i < 8; i++ {
		if first&mask == 0 {
			high := uint64(first & (mask - 1))
			return value | high<<(8*uint(i)), nil
		}
		b, err := r.byte()
		if err != nil {
			return 0, err
		}
		value |= uint64(b) << (8 * uint(i))
		mask >>= 1
	}
	return value, nil
}

// openVolumes returns a ReaderAt over the whole archive, concatenating the
// volumes of a .7z.001 set.
func openVolumes(path string) (io.ReaderAt, int64, func(), error) {
	paths := Volumes(path)
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	var parts []io.ReaderAt
	var sizes []int64
	var total int64
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			closeAll()
			return nil, 0, nil, err
		}
		files = append(files, f)
		st, err := f.Stat()
		if err != nil {
			closeAll()
			return nil, 0, nil, err
		}
		parts = append(parts, f)
		sizes = append(sizes, st.Size())
		total += st.Size()
	}
	if len(parts) == 1 {
		return parts[0], total, closeAll, nil
	}
	return &multiReaderAt{parts: parts, sizes: sizes}, total, closeAll, nil
}

type multiReaderAt struct {
	parts []io.ReaderAt
	sizes []int64
}

func (m *multiReaderAt) ReadAt(p []byte, off int64) (int, error) {
	read := 0
	for i, part := range m.parts {
		if off >= m.sizes[i] {
			off -= m.sizes[i]
			continue
		}
		for len(p) > 0 && off < m.sizes[i] {
			chunk := p
			if rest := m.sizes[i] - off; int64(len(chunk)) > rest {
				chunk = chunk[:rest]
			}
			n, err := part.ReadAt(chunk, off)
			read += n
			p = p[n:]
			off += int64(n)
			if err != nil && err != io.EOF {
				return read, err
			}
			if n == 0 {
				break
			}
		}
		if len(p) == 0 {
			return read, nil
		}
		off = 0
	}
	if len(p) > 0 {
		return read, io.EOF
	}
	return read, nil
}
