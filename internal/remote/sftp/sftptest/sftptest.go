// Package sftptest runs an in-process SSH/SFTP server rooted in a directory,
// with optional fault injection, for tests.
package sftptest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"dsfetch/internal/remote"
)

// Faults makes the first open of each file misbehave.
type Faults struct {
	DropAfter  int64 // >0: reads beyond this offset fail on the first open
	StallAfter int64 // >0: reads beyond this offset block on the first open
}

// Server serves Root read-only over SFTP.
type Server struct {
	Root     string
	User     string // default "pi"
	Password string // default "raspberry"
	HostKey  ssh.Signer
	// MoreHostKeys are offered too, as real servers have several (RSA,
	// ECDSA, Ed25519).
	MoreHostKeys []ssh.Signer
	Faults       Faults

	mu     sync.Mutex
	opened map[string]int
}

// NewHostKey generates an ed25519 host key.
func NewHostKey(t testing.TB) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// NewECDSAHostKey generates an ECDSA P-256 host key, the type x/crypto
// prefers over Ed25519.
func NewECDSAHostKey(t testing.TB) ssh.Signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Start listens on 127.0.0.1 until the test ends and returns a client config.
func (s *Server) Start(t testing.TB) remote.Config {
	t.Helper()
	if s.HostKey == nil {
		s.HostKey = NewHostKey(t)
	}
	if s.User == "" {
		s.User, s.Password = "pi", "raspberry"
	}
	s.opened = map[string]int{}
	conf := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == s.User && string(pass) == s.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	conf.AddHostKey(s.HostKey)
	for _, k := range s.MoreHostKeys {
		conf.AddHostKey(k)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go s.serveConn(nc, conf)
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	return remote.Config{Protocol: "sftp", Host: "127.0.0.1", Port: port, User: s.User, Password: s.Password, Keepalive: -1}
}

// Opens returns how many times a file was opened.
func (s *Server) Opens(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened[p]
}

func (s *Server) serveConn(nc net.Conn, conf *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, requests, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range requests {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					srv := psftp.NewRequestServer(ch, psftp.Handlers{FileGet: s, FilePut: s, FileCmd: s, FileList: s})
					_ = srv.Serve()
					ch.Close()
				}
			}
		}()
	}
}

func (s *Server) local(p string) string { return filepath.Join(s.Root, filepath.FromSlash(p)) }

// Fileread implements psftp.FileReader.
func (s *Server) Fileread(r *psftp.Request) (io.ReaderAt, error) {
	f, err := os.Open(s.local(r.Filepath))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.opened[r.Filepath]++
	first := s.opened[r.Filepath] == 1
	s.mu.Unlock()
	if first && s.Faults.DropAfter > 0 {
		return &faultReaderAt{f: f, at: s.Faults.DropAfter, drop: true}, nil
	}
	if first && s.Faults.StallAfter > 0 {
		return &faultReaderAt{f: f, at: s.Faults.StallAfter}, nil
	}
	return f, nil
}

// Filewrite implements psftp.FileWriter (read-only server).
func (s *Server) Filewrite(*psftp.Request) (io.WriterAt, error) { return nil, os.ErrPermission }

// Filecmd implements psftp.FileCmder (read-only server).
func (s *Server) Filecmd(*psftp.Request) error { return os.ErrPermission }

// Filelist implements psftp.FileLister.
func (s *Server) Filelist(r *psftp.Request) (psftp.ListerAt, error) {
	switch r.Method {
	case "List":
		entries, err := os.ReadDir(s.local(r.Filepath))
		if err != nil {
			return nil, err
		}
		var infos []os.FileInfo
		for _, e := range entries {
			if fi, err := os.Lstat(s.local(r.Filepath + "/" + e.Name())); err == nil {
				infos = append(infos, fi)
			}
		}
		return listerAt(infos), nil
	case "Stat":
		fi, err := os.Stat(s.local(r.Filepath))
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	case "Lstat":
		fi, err := os.Lstat(s.local(r.Filepath))
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
	return nil, os.ErrInvalid
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	if n+int(off) >= len(l) {
		return n, io.EOF
	}
	return n, nil
}

type faultReaderAt struct {
	f    *os.File
	at   int64
	drop bool
}

func (r *faultReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.at {
		if r.drop {
			return 0, errors.New("simulated connection drop")
		}
		time.Sleep(time.Hour)
	}
	if off+int64(len(p)) > r.at {
		n, err := r.f.ReadAt(p[:r.at-off], off)
		if err == nil {
			err = errors.New("simulated connection drop")
		}
		if !r.drop {
			err = nil
		}
		return n, err
	}
	return r.f.ReadAt(p, off)
}
