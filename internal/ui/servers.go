package ui

import (
	"fmt"
	"sync/atomic"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/store"
)

type serverRowKind int

const (
	rowAdd serverRowKind = iota
	rowLocal
	rowSettings
	rowServer
)

type serverRow struct {
	kind serverRowKind
	id   string
}

// menuRows is the number of fixed main-menu rows; a "Servers" divider
// follows them when there are servers, so the first server is at
// firstServerRow.
const (
	menuRows       = 3
	firstServerRow = menuRows + 1
)

// Servers is the start screen: a fixed menu (add server, local archives,
// settings) with the saved servers below it. A opens, Y edits the server
// under the cursor, B quits.
func (e *Env) Servers(any) (any, error) {
	e.closeSession()

	servers := e.Store.Servers()
	items := []gaba.MenuItem{
		{Text: "+  " + T("add_server"), Metadata: serverRow{kind: rowAdd}},
		{Text: iconArchive + "  " + T("local_archives"), Metadata: serverRow{kind: rowLocal}},
		{Text: iconCog + "  " + T("settings"), Metadata: serverRow{kind: rowSettings}},
	}
	if len(servers) > 0 {
		items = append(items, gaba.MenuItem{Text: T("servers_header"), Separator: true})
	}
	for _, s := range servers {
		items = append(items, gaba.MenuItem{
			Text:     fmt.Sprintf("%s  %s  · %s", iconServer, s.Title(), s.Protocol.Label()),
			Metadata: serverRow{kind: rowServer, id: s.ID},
		})
	}

	// First visit: start on the first server, which is what people open.
	if !e.serversVisited {
		e.serversVisited = true
		if len(servers) > 0 {
			e.serversSel = firstServerRow
		}
	}
	if e.serversSel >= len(items) {
		e.serversSel = len(items) - 1
	}

	// "Y Edit" only makes sense on a server row.
	var onServer atomic.Bool
	onServer.Store(e.serversSel >= firstServerRow)
	editHint := gaba.FooterHelpItem{ButtonName: "Y", HelpText: T("edit_server_short"), Show: &onServer}

	opts := gaba.DefaultListOptions("DSFetch", items)
	opts.SelectedIndex = e.serversSel
	opts.SecondaryActionButton = constants.VirtualButtonY
	opts.HelpButton = constants.VirtualButtonMenu
	opts.HelpTitle = T("help_title")
	opts.HelpText = []string{T("help_servers_a"), T("help_servers_y"), T("help_servers_b"), T("help_quit_anywhere")}
	opts.HelpExitText = T("help_exit")
	opts.FooterHelpItems = footer(btn("B", T("quit")), btn("A", T("open")), editHint)
	opts.OnSelect = func(index int, _ *gaba.MenuItem) {
		onServer.Store(index >= firstServerRow)
	}

	res, err := gaba.List(opts)
	if e.QuitRequested() || gaba.IsCancelled(err) {
		return Exit, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Selected) == 0 {
		return Nav{To: ScreenServers}, nil
	}
	idx := res.Selected[0]
	e.serversSel = idx
	row, ok := items[idx].Metadata.(serverRow)
	if !ok {
		return Nav{To: ScreenServers}, nil // a divider
	}

	if res.Action == gaba.ListActionSecondaryTriggered { // Y: edit
		if row.kind == rowServer {
			if srv, ok := e.Store.Get(row.id); ok {
				return Nav{To: ScreenServerEdit, Input: &srv}, nil
			}
		}
		return Nav{To: ScreenServers}, nil
	}

	switch row.kind {
	case rowServer:
		return Nav{To: ScreenConnect, Input: row.id}, nil
	case rowAdd:
		return Nav{To: ScreenServerEdit}, nil
	case rowLocal:
		return Nav{To: ScreenLocal}, nil
	case rowSettings:
		return Nav{To: ScreenSettings}, nil
	}
	return Nav{To: ScreenServers}, nil
}

// serverFromInput accepts *store.Server or nil.
func serverFromInput(in any) (store.Server, bool) {
	if s, ok := in.(*store.Server); ok && s != nil {
		return *s, true
	}
	return store.Server{Protocol: store.ProtoFTP, SavePassword: true, Encoding: store.EncodingUTF8}, false
}
