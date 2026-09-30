package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretBox encrypts short secrets (passwords) for storage in servers.json.
//
// Threat model: the random key file lives next to the data on the SD card,
// but the working key is derived from it together with the console's own ID
// (its CPU serial), so a card read on a computer or in another console does
// not give the passwords away; they are asked for again there. Someone with
// the console itself, or a program running on it, can still recover them:
// the device has no secure key store. Each secret is also bound to its
// context (the server it is sent to), so it cannot be moved to another
// entry that points elsewhere.
type SecretBox interface {
	Seal(plain string, context []byte) (string, error)
	Open(enc string, context []byte) (string, error)
}

const (
	sealedPrefix       = "v2:" // device-bound key, bound to its context
	legacySealedPrefix = "v1:" // the key file alone, no context (read only)
)

var ErrBadCiphertext = errors.New("store: cannot decrypt secret")

// AESBox is AES-256-GCM with a random 96-bit nonce per secret.
type AESBox struct {
	aead   cipher.AEAD // v2
	legacy cipher.AEAD // v1
}

// NewAESBox builds a box from the 32-byte key file and the device ID (nil
// where there is none, e.g. a computer in dev mode).
func NewAESBox(fileKey, deviceID []byte) (*AESBox, error) {
	if len(fileKey) != 32 {
		return nil, fmt.Errorf("store: key must be 32 bytes, got %d", len(fileKey))
	}
	key, err := hkdf.Key(sha256.New, fileKey, deviceID, "dsfetch servers.json passwords v2", 32)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	legacy, err := newGCM(fileKey)
	if err != nil {
		return nil, err
	}
	return &AESBox{aead: aead, legacy: legacy}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal returns "v2:" + base64(nonce || ciphertext || tag). context is
// authenticated, not stored: Open needs the same one.
func (b *AESBox) Seal(plain string, context []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), context)
	return sealedPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal. It also reads v1 secrets, which have no context.
func (b *AESBox) Open(enc string, context []byte) (string, error) {
	aead, data := b.aead, ""
	switch {
	case strings.HasPrefix(enc, sealedPrefix):
		data = strings.TrimPrefix(enc, sealedPrefix)
	case strings.HasPrefix(enc, legacySealedPrefix):
		aead, data, context = b.legacy, strings.TrimPrefix(enc, legacySealedPrefix), nil
	default:
		return "", ErrBadCiphertext
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", ErrBadCiphertext
	}
	ns := aead.NonceSize()
	if len(raw) < ns+aead.Overhead() {
		return "", ErrBadCiphertext
	}
	plain, err := aead.Open(nil, raw[:ns], raw[ns:], context)
	if err != nil {
		return "", ErrBadCiphertext
	}
	return string(plain), nil
}

// IsLegacy reports a v1 secret, which should be sealed again.
func IsLegacy(enc string) bool { return strings.HasPrefix(enc, legacySealedPrefix) }

// ErrBadKey: the key file exists but is not a 32-byte key (damaged).
var ErrBadKey = errors.New("store: damaged key file")

// LoadOrCreateKey reads a 32-byte key from path, creating it (mode 0600)
// with random bytes if it does not exist.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%w: %s: expected 32 bytes, got %d", ErrBadKey, path, len(key))
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
