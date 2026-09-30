// Package ftptest runs an in-process FTP server for tests, with optional
// fault injection (dropped or stalled transfers).
package ftptest

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"dsfetch/internal/remote"
)

// Faults makes the first read of each file misbehave.
type Faults struct {
	DropAfter  int64 // >0: first open of a file fails after this many bytes
	StallAfter int64 // >0: first open of a file blocks forever after this many bytes
}

// Server configuration.
type Server struct {
	FS        afero.Fs
	User      string // default "test"
	Password  string // default "test"
	AllowAnon bool
	TLS       *tls.Config
	Faults    Faults

	// RequireSessionReuse refuses TLS data connections that do not resume
	// a TLS session, like vsftpd (require_ssl_reuse=YES, its default) and
	// ProFTPD do.
	RequireSessionReuse bool

	settings *ftpserver.Settings
	tlsConf  *tls.Config
	mu       sync.Mutex
	opened   map[string]int
}

// Start runs the server until the test ends and returns a client config.
func (s *Server) Start(t testing.TB) remote.Config {
	t.Helper()
	if s.User == "" {
		s.User, s.Password = "test", "test"
	}
	if s.FS == nil {
		s.FS = afero.NewMemMapFs()
	}
	s.opened = map[string]int{}
	s.settings = &ftpserver.Settings{ListenAddr: "127.0.0.1:0", ConnectionTimeout: 5, IdleTimeout: 60}
	if s.TLS != nil {
		s.settings.TLSRequired = ftpserver.MandatoryEncryption
	}
	srv := ftpserver.NewFtpServer(s)
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Stop() })

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	s.tlsConf = s.serverTLS(portStr)
	return remote.Config{Protocol: "ftp", Host: host, Port: port, User: s.User, Password: s.Password, Keepalive: -1}
}

// Opens returns how many times a file was opened for reading.
func (s *Server) Opens(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened[name]
}

func (s *Server) GetSettings() (*ftpserver.Settings, error) { return s.settings, nil }
func (s *Server) ClientConnected(ftpserver.ClientContext) (string, error) {
	return "dsfetch test server", nil
}
func (s *Server) ClientDisconnected(ftpserver.ClientContext) {}
func (s *Server) GetTLSConfig() (*tls.Config, error) {
	if s.tlsConf == nil {
		return nil, errors.New("TLS not configured")
	}
	return s.tlsConf, nil
}

// serverTLS returns the TLS config for the control connection (on
// controlPort) and the data connections.
func (s *Server) serverTLS(controlPort string) *tls.Config {
	if s.TLS == nil || !s.RequireSessionReuse {
		return s.TLS
	}
	data := s.TLS.Clone()
	data.VerifyConnection = func(cs tls.ConnectionState) error {
		if !cs.DidResume {
			return errors.New("SSL connection failed: session reuse required")
		}
		return nil
	}
	conf := s.TLS.Clone()
	conf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if _, port, _ := net.SplitHostPort(hello.Conn.LocalAddr().String()); port == controlPort {
			return nil, nil // the control connection: a full handshake is fine
		}
		return data, nil // session tickets stay those of conf
	}
	return conf
}

func (s *Server) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user == "anonymous" && s.AllowAnon {
		return &faultFs{Fs: s.FS, srv: s}, nil
	}
	if user != s.User || pass != s.Password {
		return nil, errors.New("bad credentials")
	}
	return &faultFs{Fs: s.FS, srv: s}, nil
}

type faultFs struct {
	afero.Fs
	srv *Server
}

func (f *faultFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	file, err := f.Fs.OpenFile(name, flag, perm)
	if err != nil || flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		return file, err
	}
	return f.wrap(name, file), nil
}

func (f *faultFs) Open(name string) (afero.File, error) {
	file, err := f.Fs.Open(name)
	if err != nil {
		return nil, err
	}
	return f.wrap(name, file), nil
}

func (f *faultFs) wrap(name string, file afero.File) afero.File {
	if st, err := file.Stat(); err == nil && st.IsDir() {
		return file
	}
	f.srv.mu.Lock()
	f.srv.opened[name]++
	first := f.srv.opened[name] == 1
	f.srv.mu.Unlock()
	if !first {
		return file
	}
	switch {
	case f.srv.Faults.DropAfter > 0:
		return &faultFile{File: file, left: f.srv.Faults.DropAfter, drop: true}
	case f.srv.Faults.StallAfter > 0:
		return &faultFile{File: file, left: f.srv.Faults.StallAfter}
	}
	return file
}

type faultFile struct {
	afero.File
	left int64
	drop bool
}

func (f *faultFile) Read(p []byte) (int, error) {
	if f.left <= 0 {
		if f.drop {
			return 0, errors.New("simulated connection drop")
		}
		time.Sleep(time.Hour)
	}
	if int64(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.File.Read(p)
	f.left -= int64(n)
	return n, err
}
