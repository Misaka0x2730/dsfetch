package ui

import (
	"context"
	"errors"
	"log/slog"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"

	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
	"dsfetch/internal/remote/backends"
	"dsfetch/internal/store"
)

// Connect logs in to a server (input: server ID) and opens the browser.
func (e *Env) Connect(in any) (any, error) {
	id, _ := in.(string)
	srv, ok := e.Store.Get(id)
	if !ok {
		return Nav{To: ScreenServers}, nil
	}
	if !platform.HasNetwork() {
		info(T("err_no_network"))
		return Nav{To: ScreenServers}, nil
	}

	needsPassword := !(srv.Anonymous && srv.Protocol != store.ProtoSFTP)
	password := ""
	if needsPassword {
		if p, ok := e.Store.Password(srv); ok {
			password = p
		} else if p, ok := e.passwords[id]; ok {
			password = p
		} else {
			p, ok := e.askPassword(&srv)
			if !ok {
				return Nav{To: ScreenServers}, nil
			}
			password = p
		}
	}

	for {
		be, err := backends.New(remoteConfig(srv, password))
		if err != nil {
			info(errorMessage(T("err_connect", srv.Title()), err))
			return Nav{To: ScreenServers}, nil
		}
		cerr := e.runTask(T("connecting_to", srv.Title()), true, func(ctx context.Context) error {
			return be.Connect(ctx)
		})
		if e.QuitRequested() {
			_ = be.Close()
			return Exit, nil
		}
		if cerr != nil {
			_ = be.Close()
			var mismatch *remote.HostKeyMismatchError
			switch {
			case errors.Is(cerr, context.Canceled):
				return Nav{To: ScreenServers}, nil
			case errors.Is(cerr, remote.ErrAuth) && needsPassword:
				if !confirm(T("err_auth")+"\n\n"+T("retry_password_q"), T("retry"), T("back")) {
					return Nav{To: ScreenServers}, nil
				}
				p, ok := e.askPassword(&srv)
				if !ok {
					return Nav{To: ScreenServers}, nil
				}
				password = p
				continue
			case errors.As(cerr, &mismatch):
				if !confirm(T("hostkey_changed_q", srv.Title(), mismatch.Known, mismatch.Got), T("trust"), T("cancel")) {
					return Nav{To: ScreenServers}, nil
				}
				srv.HostKey = mismatch.Got
				if err := e.Store.Update(srv.ID, func(s *store.Server) { s.HostKey = mismatch.Got }); err != nil {
					slog.Error("save host key", "err", err)
				}
				continue
			default:
				slog.Warn("connect failed", "server", srv.Host, "err", cerr)
				info(errorMessage(T("err_connect", srv.Title()), cerr))
				return Nav{To: ScreenServers}, nil
			}
		}

		// Trust on first use: pin the key we just saw. The backend accepted
		// it, so a different value is the first pin or an older form of the
		// same key (SFTP pins without the key type).
		if fp, ok := be.(remote.Fingerprinter); ok && fp.Fingerprint() != "" && srv.HostKey != fp.Fingerprint() {
			srv.HostKey = fp.Fingerprint()
			if err := e.Store.Update(srv.ID, func(s *store.Server) { s.HostKey = srv.HostKey }); err != nil {
				slog.Error("save host key", "err", err)
			}
			slog.Info("pinned server key", "server", srv.Host, "fingerprint", srv.HostKey)
		}
		if !srv.SavePassword && password != "" {
			e.rememberPassword(srv.ID, password)
		}
		e.session = &Session{Server: srv, Password: password, Backend: be, cursor: map[string]listPos{}}
		start := "/"
		if srv.InitialPath != "" {
			start = srv.InitialPath
		}
		slog.Info("connected", "server", srv.Host, "protocol", srv.Protocol)
		return Nav{To: ScreenBrowser, Input: start}, nil
	}
}

// askPassword shows a small login form (gabagool's keyboard has no prompt
// line, so the form provides the context and masks the password).
func (e *Env) askPassword(srv *store.Server) (string, bool) {
	const (
		rowPassword = iota
		rowRemember
		rowConnect
	)
	items := []gaba.ItemWithOptions{
		{Item: gaba.MenuItem{Text: T("f_password")}, Options: []gaba.Option{kbOption("", gaba.KeyboardLayoutGeneral, true)}},
		{Item: gaba.MenuItem{Text: T("f_remember")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(srv.SavePassword)},
		{Item: gaba.MenuItem{Text: T("connect")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}},
	}
	who := srv.Host
	if srv.Username != "" {
		who = srv.Username + "@" + srv.Host
	}
	settings := gaba.OptionListSettings{
		FooterHelpItems: footer(btn("B", T("cancel")), btn("A", T("change")), startBtn(T("connect"))),
		HelpExitText:    T("help_exit"),
	}
	for {
		res, err := gaba.OptionsList(T("password_for", who), settings, items)
		if err != nil || e.QuitRequested() {
			return "", false
		}
		if res.Action != gaba.ListActionConfirmed && res.Selected != rowConnect {
			continue
		}
		pw, _ := items[rowPassword].Options[items[rowPassword].SelectedOption].Value.(string)
		remember := optBool(items[rowRemember])
		if remember != srv.SavePassword || remember {
			srv.SavePassword = remember
			if err := e.Store.SetPassword(srv, pw); err == nil {
				_ = e.Store.Update(srv.ID, func(s *store.Server) {
					s.SavePassword = srv.SavePassword
					s.PasswordEnc = srv.PasswordEnc
				})
			}
		}
		return pw, true
	}
}
