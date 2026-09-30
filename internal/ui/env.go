// Package ui implements the screens. Each screen is a function that blocks
// on gabagool components and returns a Nav telling the router where to go.
package ui

import (
	"context"
	"log/slog"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/router"

	"dsfetch/internal/i18n"
	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
	"dsfetch/internal/remote/backends"
	"dsfetch/internal/store"
)

// Screens.
const (
	ScreenServers router.Screen = iota
	ScreenServerEdit
	ScreenConnect
	ScreenBrowser
	ScreenTarget
	ScreenQueue
	ScreenLocal
	ScreenSettings
)

// Nav is what every screen returns: the next screen and its input.
type Nav struct {
	To    router.Screen
	Input any
}

// Exit ends the application.
var Exit = Nav{To: router.ScreenExit}

// Env holds dependencies and cross-screen state.
type Env struct {
	Platform *platform.Platform
	Store    *store.Store
	Settings store.Settings
	DataDir  string
	// SevenZipBinary is the bundled 7zz, "" if absent.
	SevenZipBinary string
	// QuitRequested reports an SDL quit (window closed, SIGTERM).
	QuitRequested func() bool

	session        *Session
	serversSel     int
	serversVisited bool
	localCursor    map[string]listPos
	// passwords typed this run for servers that do not save them.
	passwords map[string]string
}

func (e *Env) rememberPassword(id, pw string) {
	if e.passwords == nil {
		e.passwords = map[string]string{}
	}
	e.passwords[id] = pw
}

// sevenZipBinary returns the 7zz path unless the built-in engine is chosen.
func (e *Env) sevenZipBinary() string {
	if e.Settings.SevenZipEngine == store.SevenZipBuiltin {
		return ""
	}
	return e.SevenZipBinary
}

// T is a shorthand for i18n.T.
func T(id string, args ...any) string { return i18n.T(id, args...) }

// ApplyLanguage translates the parts gabagool draws itself (the on-screen
// keyboard's footer and help).
func ApplyLanguage() {
	gaba.SetKeyboardLabels(gaba.KeyboardLabels{
		Delete:      T("kb_delete"),
		Space:       T("kb_space"),
		Symbols:     T("kb_symbols"),
		Shift:       T("kb_shift"),
		Cancel:      T("kb_cancel"),
		OK:          T("kb_ok"),
		HelpTitle:   T("kb_help_title"),
		HelpGeneral: helpLines("kb_help_move", "kb_help_type", "kb_help_delete", "kb_help_space", "kb_help_shift", "kb_help_cursor", "kb_help_cancel", "kb_help_ok"),
		HelpURL:     helpLines("kb_help_move", "kb_help_type", "kb_help_delete", "kb_help_symbols", "kb_help_shift", "kb_help_cursor", "kb_help_cancel", "kb_help_ok"),
		HelpNumeric: helpLines("kb_help_move", "kb_help_type", "kb_help_delete", "kb_help_cursor", "kb_help_cancel", "kb_help_ok"),
	})
}

func helpLines(ids ...string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "• " + T(id)
	}
	return out
}

// SaveSettings persists and applies settings.
func (e *Env) SaveSettings(s store.Settings) {
	e.Settings = s
	e.Platform.SetLidSleep(s.LidSleep)
	e.Platform.SetIdleSleep(s.IdleSleep)
	i18n.SetLanguage(s.Language)
	ApplyLanguage()
	if err := store.SaveSettings(e.DataDir, s); err != nil {
		slog.Error("save settings", "err", err)
	}
}

// Session is an open connection to a server for browsing.
type Session struct {
	Server   store.Server
	Password string // kept in memory for reconnects, never written unless SavePassword
	Backend  remote.Backend
	Dir      string
	// cursor remembers the list position per folder.
	cursor map[string]listPos
}

type listPos struct{ selected, visibleStart int }

// Config builds the backend configuration for the session's server.
func (s *Session) Config() remote.Config {
	return remoteConfig(s.Server, s.Password)
}

func remoteConfig(srv store.Server, password string) remote.Config {
	return remote.Config{
		Protocol:         string(srv.Protocol),
		Host:             strings.TrimSpace(srv.Host),
		Port:             srv.EffectivePort(),
		User:             srv.Username,
		Password:         password,
		Anonymous:        srv.Anonymous,
		Domain:           srv.Domain,
		TLS:              srv.TLS,
		Encoding:         srv.Encoding,
		KnownFingerprint: srv.HostKey,
	}
}

// NewBackend opens a fresh, connected backend with the session's settings
// (the downloader uses its own connection).
func (s *Session) NewBackend(ctx context.Context) (remote.Backend, error) {
	be, err := backends.New(s.Config())
	if err != nil {
		return nil, err
	}
	if err := be.Connect(ctx); err != nil {
		return nil, err
	}
	return be, nil
}

// closeSession drops the browsing connection.
func (e *Env) closeSession() {
	if e.session != nil && e.session.Backend != nil {
		_ = e.session.Backend.Close()
	}
	e.session = nil
}

// Shutdown releases resources before exit.
func (e *Env) Shutdown() { e.closeSession() }
