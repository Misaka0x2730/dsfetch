// Package store persists the server list (data/servers.json, mode 0600, with
// passwords encrypted by a SecretBox) and application settings.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Protocol of a server entry.
type Protocol string

const (
	ProtoFTP  Protocol = "ftp"
	ProtoSFTP Protocol = "sftp"
	ProtoSMB  Protocol = "smb"
)

// Protocols in the order they are offered in the UI.
var Protocols = []Protocol{ProtoFTP, ProtoSFTP, ProtoSMB}

// DefaultPort for a protocol.
func (p Protocol) DefaultPort() int {
	switch p {
	case ProtoSFTP:
		return 22
	case ProtoSMB:
		return 445
	default:
		return 21
	}
}

// Label is the display name.
func (p Protocol) Label() string {
	switch p {
	case ProtoSFTP:
		return "SFTP"
	case ProtoSMB:
		return "SMB"
	default:
		return "FTP"
	}
}

// FTP filename encodings (old servers send Cyrillic in a code page).
const (
	EncodingUTF8   = "utf-8"
	EncodingCP1251 = "cp1251"
	EncodingCP866  = "cp866"
)

// Encodings offered for FTP servers.
var Encodings = []string{EncodingUTF8, EncodingCP1251, EncodingCP866}

// Server is one saved connection.
type Server struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Protocol Protocol `json:"protocol"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`

	// Anonymous means FTP "anonymous" login or SMB guest access.
	Anonymous    bool   `json:"anonymous,omitempty"`
	Username     string `json:"username,omitempty"`
	PasswordEnc  string `json:"password_enc,omitempty"`
	SavePassword bool   `json:"save_password"`
	Domain       string `json:"domain,omitempty"` // SMB

	InitialPath string `json:"initial_path,omitempty"`

	// FTP options. Passive mode (EPSV, then PASV) is always used; the FTP
	// library has no active mode.
	TLS      bool   `json:"tls,omitempty"`      // explicit FTPS (AUTH TLS)
	Encoding string `json:"encoding,omitempty"` // EncodingUTF8 (default), EncodingCP1251, EncodingCP866

	// HostKey is the pinned SHA256 fingerprint: the SSH host key for SFTP,
	// the TLS certificate for FTPS (trust on first use).
	HostKey string `json:"host_key,omitempty"`

	// LastTargets remembers the local folder chosen for each remote folder.
	LastTargets map[string]string `json:"last_targets,omitempty"`
}

// Title is what the server list shows.
func (s Server) Title() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Host
}

// EffectivePort falls back to the protocol default.
func (s Server) EffectivePort() int {
	if s.Port > 0 {
		return s.Port
	}
	return s.Protocol.DefaultPort()
}

type serversFile struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

// Store is the server list backed by <dir>/servers.json.
type Store struct {
	path string
	box  SecretBox

	mu      sync.Mutex
	servers []Server

	setAside []string // damaged files Open renamed to .bad
}

// BadSuffix is added to a damaged data file that was set aside, so that
// DSFetch can start without it and the file can still be mended by hand.
const BadSuffix = ".bad"

// Open loads <dir>/servers.json (creating nothing until the first save) and
// the encryption key <dir>/.key (created on first use). deviceID binds saved
// passwords to this console (see SecretBox); nil where there is none.
// Passwords saved by older versions are sealed again the new way. A damaged
// servers.json or .key is renamed to .bad and Open starts without it (see
// SetAside).
func Open(dir string, deviceID []byte) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "servers.json")}
	keyPath := filepath.Join(dir, ".key")
	key, err := LoadOrCreateKey(keyPath)
	if errors.Is(err, ErrBadKey) {
		// A new key: passwords saved with the old one cannot be read any
		// more, and the UI asks for them again.
		if err := s.putAside(keyPath, err); err != nil {
			return nil, err
		}
		key, err = LoadOrCreateKey(keyPath)
	}
	if err != nil {
		return nil, err
	}
	if s.box, err = NewAESBox(key, deviceID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var f serversFile
		if err := json.Unmarshal(data, &f); err != nil {
			if err := s.putAside(s.path, fmt.Errorf("%s: %w", s.path, err)); err != nil {
				return nil, err
			}
			break
		}
		s.servers = f.Servers
	}
	if s.resealLegacy() > 0 {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// putAside renames a damaged file to <name>.bad (replacing an older one);
// cause is returned when even that fails.
func (s *Store) putAside(path string, cause error) error {
	if err := os.Rename(path, path+BadSuffix); err != nil {
		return cause
	}
	slog.Warn("damaged file renamed", "to", path+BadSuffix, "err", cause)
	s.setAside = append(s.setAside, filepath.Base(path))
	return nil
}

// SetAside returns the names of the damaged files Open renamed to .bad.
func (s *Store) SetAside() []string { return s.setAside }

// resealLegacy upgrades v1 passwords (the key file alone) to v2 (bound to
// this console and their server). Ones that cannot be decrypted are left:
// the UI asks for them.
func (s *Store) resealLegacy() int {
	n := 0
	for i := range s.servers {
		srv := &s.servers[i]
		if !IsLegacy(srv.PasswordEnc) {
			continue
		}
		plain, err := s.box.Open(srv.PasswordEnc, nil)
		if err != nil {
			continue
		}
		if enc, err := s.box.Seal(plain, secretContext(*srv)); err == nil {
			srv.PasswordEnc = enc
			n++
		}
	}
	return n
}

// secretContext binds a saved password to where it is sent: protocol, host,
// port and user. The editor seals it again when any of them changes.
func secretContext(srv Server) []byte {
	return []byte(strings.Join([]string{
		string(srv.Protocol), strings.ToLower(srv.Host), strconv.Itoa(srv.EffectivePort()), srv.Username,
	}, "\x00"))
}

// Servers returns a copy of the list, sorted by title.
func (s *Store) Servers() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, len(s.servers))
	for i, srv := range s.servers {
		out[i] = cloneServer(srv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Title()) < strings.ToLower(out[j].Title())
	})
	return out
}

// Get finds a server by ID.
func (s *Store) Get(id string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.servers {
		if srv.ID == id {
			return cloneServer(srv), true
		}
	}
	return Server{}, false
}

// Put inserts or replaces a server (by ID, assigning one if empty) and saves.
func (s *Store) Put(srv Server) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if srv.ID == "" {
		srv.ID = newID()
	}
	srv = cloneServer(srv)
	replaced := false
	for i := range s.servers {
		if s.servers[i].ID == srv.ID {
			s.servers[i] = srv
			replaced = true
			break
		}
	}
	if !replaced {
		s.servers = append(s.servers, srv)
	}
	return cloneServer(srv), s.saveLocked()
}

// Update applies fn to a stored server and saves.
func (s *Store) Update(id string, fn func(*Server)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.servers {
		if s.servers[i].ID == id {
			fn(&s.servers[i])
			return s.saveLocked()
		}
	}
	return fmt.Errorf("store: server %q not found", id)
}

// Delete removes a server and saves.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.servers[:0]
	for _, srv := range s.servers {
		if srv.ID != id {
			out = append(out, srv)
		}
	}
	s.servers = out
	return s.saveLocked()
}

// SetPassword stores plain encrypted in srv if srv.SavePassword is set,
// otherwise clears any stored password. It does not save the store.
func (s *Store) SetPassword(srv *Server, plain string) error {
	if !srv.SavePassword || plain == "" {
		srv.PasswordEnc = ""
		return nil
	}
	enc, err := s.box.Seal(plain, secretContext(*srv))
	if err != nil {
		return err
	}
	srv.PasswordEnc = enc
	return nil
}

// Password decrypts the stored password. ok is false if there is none or it
// cannot be decrypted (data/.key lost, the card moved to another console, or
// the entry's address edited outside DSFetch), in which case the UI asks.
func (s *Store) Password(srv Server) (plain string, ok bool) {
	if srv.PasswordEnc == "" {
		return "", false
	}
	p, err := s.box.Open(srv.PasswordEnc, secretContext(srv))
	if err != nil {
		return "", false
	}
	return p, true
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(serversFile{Version: 1, Servers: s.servers}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(data, '\n'), 0o600)
}

func cloneServer(srv Server) Server {
	if srv.LastTargets != nil {
		m := make(map[string]string, len(srv.LastTargets))
		for k, v := range srv.LastTargets {
			m[k] = v
		}
		srv.LastTargets = m
	}
	return srv
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeFileAtomic writes via a temp file + fsync + rename, so a power loss
// never leaves a truncated servers.json.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Some filesystems (FAT) ignore chmod; that is fine.
	_ = os.Chmod(path, perm)
	return nil
}
