package ui

import (
	"strconv"
	"strings"
	"sync/atomic"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/remote"
	"dsfetch/internal/store"
)

// Form rows of the server editor.
const (
	fName = iota
	fProtocol
	fHost
	fPort
	fAnon
	fUser
	fPassword
	fRemember
	fDomain
	fStartDir
	fTLS
	fEncoding
	fSave
	fDelete // existing servers only
)

// Host shortcuts: in symbol mode (digits) the shortcut row types ".", so an
// IP address needs no mode switching between digits and dots.
var hostShortcuts = []gaba.URLShortcut{
	{Value: "192.168.", SymbolValue: "."},
	{Value: ".", SymbolValue: "."},
	{Value: ".local", SymbolValue: ":"},
	{Value: ".lan", SymbolValue: "-"},
	{Value: "10.0.", SymbolValue: "_"},
}

func kbOption(value string, layout gaba.KeyboardLayout, masked bool) gaba.Option {
	return gaba.Option{
		DisplayName:    value,
		Value:          value,
		Type:           gaba.OptionTypeKeyboard,
		KeyboardPrompt: value,
		KeyboardLayout: layout,
		Masked:         masked,
	}
}

func hostOption(value string) gaba.Option {
	o := kbOption(value, gaba.KeyboardLayoutURL, false)
	o.URLShortcuts = hostShortcuts
	return o
}

func yesNoOptions(onUpdate func(bool)) []gaba.Option {
	upd := func(v interface{}) {
		if onUpdate != nil {
			onUpdate(v.(bool))
		}
	}
	return []gaba.Option{
		{DisplayName: T("no"), Value: false, OnUpdate: upd},
		{DisplayName: T("yes"), Value: true, OnUpdate: upd},
	}
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

func optString(it gaba.ItemWithOptions) string {
	if len(it.Options) == 0 {
		return ""
	}
	s, _ := it.Options[it.SelectedOption].Value.(string)
	return strings.TrimSpace(s)
}

func optBool(it gaba.ItemWithOptions) bool {
	if len(it.Options) == 0 {
		return false
	}
	b, _ := it.Options[it.SelectedOption].Value.(bool)
	return b
}

// plainFTPLogin reports an FTP server that logs in with a user name and
// password without TLS: both cross the network unencrypted.
func plainFTPLogin(s store.Server) bool {
	return s.Protocol == store.ProtoFTP && !s.TLS && !s.Anonymous
}

// ServerEdit adds (nil input) or edits (*store.Server) a server.
func (e *Env) ServerEdit(in any) (any, error) {
	srv, existing := serverFromInput(in)
	password, _ := e.Store.Password(srv)

	proto := srv.Protocol
	anon := srv.Anonymous
	var ftpOnly, smbOnly, anonAllowed, creds atomic.Bool
	refresh := func() {
		ftpOnly.Store(proto == store.ProtoFTP)
		smbOnly.Store(proto == store.ProtoSMB)
		anonAllowed.Store(proto != store.ProtoSFTP)
		creds.Store(!(anon && proto != store.ProtoSFTP))
	}
	refresh()

	port := ""
	if srv.Port > 0 {
		port = strconv.Itoa(srv.Port)
	} else {
		port = strconv.Itoa(proto.DefaultPort())
	}

	items := make([]gaba.ItemWithOptions, fSave+1, fDelete+1)

	protoOpts := make([]gaba.Option, len(store.Protocols))
	protoSel := 0
	for i, p := range store.Protocols {
		p := p
		if p == proto {
			protoSel = i
		}
		protoOpts[i] = gaba.Option{DisplayName: p.Label(), Value: p, OnUpdate: func(v interface{}) {
			old := proto
			proto = v.(store.Protocol)
			refresh()
			// Follow the protocol's default port unless the user set another.
			cur := optString(items[fPort])
			if cur == "" || cur == strconv.Itoa(old.DefaultPort()) {
				items[fPort].Options[0] = kbOption(strconv.Itoa(proto.DefaultPort()), gaba.KeyboardLayoutNumeric, false)
			}
		}}
	}

	encOpts := make([]gaba.Option, len(store.Encodings))
	encSel := 0
	for i, enc := range store.Encodings {
		if enc == srv.Encoding {
			encSel = i
		}
		encOpts[i] = gaba.Option{DisplayName: strings.ToUpper(enc), Value: enc}
	}

	items[fName] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_name")}, Options: []gaba.Option{kbOption(srv.Name, gaba.KeyboardLayoutGeneral, false)}}
	items[fProtocol] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_protocol")}, Options: protoOpts, SelectedOption: protoSel}
	items[fHost] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_host")}, Options: []gaba.Option{hostOption(srv.Host)}}
	items[fPort] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_port")}, Options: []gaba.Option{kbOption(port, gaba.KeyboardLayoutNumeric, false)}}
	items[fAnon] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_anonymous")}, Options: yesNoOptions(func(b bool) { anon = b; refresh() }), SelectedOption: boolIndex(anon), VisibleWhen: &anonAllowed}
	items[fUser] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_user")}, Options: []gaba.Option{kbOption(srv.Username, gaba.KeyboardLayoutGeneral, false)}, VisibleWhen: &creds}
	items[fPassword] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_password")}, Options: []gaba.Option{kbOption(password, gaba.KeyboardLayoutGeneral, true)}, VisibleWhen: &creds}
	items[fRemember] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_remember")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(srv.SavePassword), VisibleWhen: &creds}
	items[fDomain] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_domain")}, Options: []gaba.Option{kbOption(srv.Domain, gaba.KeyboardLayoutGeneral, false)}, VisibleWhen: &smbOnly}
	items[fStartDir] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_start_dir")}, Options: []gaba.Option{kbOption(srv.InitialPath, gaba.KeyboardLayoutGeneral, false)}}
	items[fTLS] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_ftps")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(srv.TLS), VisibleWhen: &ftpOnly}
	items[fEncoding] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_encoding")}, Options: encOpts, SelectedOption: encSel, VisibleWhen: &ftpOnly}
	items[fSave] = gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("save")}, Options: []gaba.Option{{DisplayName: "", Type: gaba.OptionTypeClickable}}}
	if existing {
		items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("delete_server")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}})
	} else {
		items = items[:fSave+1]
	}

	title := T("new_server")
	if existing {
		title = T("edit_server")
	}
	settings := gaba.OptionListSettings{
		FooterHelpItems:  footer(btn("B", T("cancel")), btn("A", T("change")), startBtn(T("save"))),
		ListPickerButton: constants.VirtualButtonA,
		HelpExitText:     T("help_exit"),
	}

	for {
		res, err := gaba.OptionsList(title, settings, items)
		if e.QuitRequested() {
			return Exit, nil
		}
		if gaba.IsCancelled(err) {
			return Nav{To: ScreenServers}, nil
		}
		if err != nil {
			return nil, err
		}
		if existing && res.Action == gaba.ListActionSelected && res.Selected == fDelete {
			settings.InitialSelectedIndex = fDelete
			if confirm(T("delete_server_q", srv.Title()), T("delete"), T("cancel")) {
				if err := e.Store.Delete(srv.ID); err != nil {
					info(errorMessage(T("err_save"), err))
					continue
				}
				if e.serversSel > firstServerRow {
					e.serversSel--
				}
				return Nav{To: ScreenServers}, nil
			}
			continue
		}
		if res.Action != gaba.ListActionConfirmed && res.Selected != fSave {
			continue
		}
		settings.InitialSelectedIndex = res.Selected
		settings.VisibleStartIndex = res.VisibleStartIndex

		out := srv
		out.Protocol = proto
		out.Name = optString(items[fName])
		out.Host = optString(items[fHost])
		out.Port, _ = strconv.Atoi(optString(items[fPort]))
		out.Anonymous = anon && proto != store.ProtoSFTP
		out.Username = optString(items[fUser])
		out.SavePassword = optBool(items[fRemember])
		out.Domain = optString(items[fDomain])
		out.InitialPath = optString(items[fStartDir])
		out.TLS = optBool(items[fTLS])
		out.Encoding, _ = items[fEncoding].Options[items[fEncoding].SelectedOption].Value.(string)
		pw, _ := items[fPassword].Options[items[fPassword].SelectedOption].Value.(string)

		if out.Host == "" {
			info(T("err_host_required"))
			continue
		}
		if out.Port <= 0 || out.Port > 65535 {
			out.Port = proto.DefaultPort()
		}
		if out.InitialPath != "" {
			out.InitialPath = remote.Clean(out.InitialPath)
		}
		if out.Anonymous {
			out.Username, pw = "", ""
		}
		if plainFTPLogin(out) && !confirm(T("warn_ftp_plain"), T("save_anyway"), T("back")) {
			continue
		}
		// A changed address may be another server: forget the pinned key.
		// The remembered target folders stay (usually it is the same NAS
		// at a new address, and they are keyed by remote paths anyway).
		if existing && (out.Host != srv.Host || out.Port != srv.Port || out.Protocol != srv.Protocol) {
			out.HostKey = ""
		}
		if err := e.Store.SetPassword(&out, pw); err != nil {
			info(errorMessage(T("err_save"), err))
			continue
		}
		saved, err := e.Store.Put(out)
		if err != nil {
			info(errorMessage(T("err_save"), err))
			continue
		}
		if !saved.SavePassword && pw != "" {
			e.rememberPassword(saved.ID, pw)
		}
		for i, s := range e.Store.Servers() {
			if s.ID == saved.ID {
				e.serversSel = firstServerRow + i
				e.serversVisited = true
			}
		}
		return Nav{To: ScreenServers}, nil
	}
}
