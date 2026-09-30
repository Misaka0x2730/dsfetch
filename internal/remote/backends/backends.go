// Package backends constructs a remote.Backend for a protocol.
package backends

import (
	"fmt"

	"dsfetch/internal/remote"
	"dsfetch/internal/remote/ftp"
	"dsfetch/internal/remote/sftp"
	"dsfetch/internal/remote/smb"
)

// New returns an unconnected backend for cfg.Protocol.
func New(cfg remote.Config) (remote.Backend, error) {
	switch cfg.Protocol {
	case "ftp":
		return ftp.New(cfg), nil
	case "sftp":
		return sftp.New(cfg), nil
	case "smb":
		return smb.New(cfg), nil
	default:
		return nil, fmt.Errorf("unknown protocol %q", cfg.Protocol)
	}
}
