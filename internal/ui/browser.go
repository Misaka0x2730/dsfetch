package ui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/remote"
)

// Selection is what the user picked for download in one folder.
type Selection struct {
	Dir     string
	Entries []remote.Entry
}

// Browser lists a remote folder (input: path).
func (e *Env) Browser(in any) (any, error) {
	s := e.session
	if s == nil {
		return Nav{To: ScreenServers}, nil
	}
	dir, _ := in.(string)
	dir = remote.Clean(dir)
	s.Dir = dir

	entries, err := e.listRemote(dir)
	if e.QuitRequested() {
		return Exit, nil
	}
	if errors.Is(err, context.Canceled) {
		return e.browserBack(dir), nil
	}
	if err != nil {
		info(errorMessage(T("err_list", dir), err))
		if dir != "/" && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)) {
			return Nav{To: ScreenBrowser, Input: remote.Parent(dir)}, nil
		}
		return Nav{To: ScreenServers}, nil
	}

	items := make([]gaba.MenuItem, len(entries))
	for i, ent := range entries {
		items[i] = gaba.MenuItem{Text: entryLabel(ent)}
	}
	title := s.Server.Title() + ":" + dir
	opts := gaba.DefaultListOptions(title, items)
	opts.UseSmallTitle = true
	opts.EmptyMessage = T("empty_folder")
	if pos, ok := s.cursor[dir]; ok {
		opts.SelectedIndex = pos.selected
		opts.VisibleStartIndex = pos.visibleStart
	}
	opts.MultiSelectButton = constants.VirtualButtonSelect
	opts.MultiSelectConfirmButton = constants.VirtualButtonUnassigned
	opts.ActionButton = constants.VirtualButtonX
	opts.SecondaryActionButton = constants.VirtualButtonY
	opts.HelpButton = constants.VirtualButtonMenu
	opts.HelpTitle = T("help_title")
	opts.HelpText = []string{T("help_browser_a"), T("help_browser_x"), T("help_browser_select"), T("help_browser_y"), T("help_browser_b")}
	opts.HelpExitText = T("help_exit")
	opts.FooterHelpItems = footer(btn("B", T("back")), btn("A", T("open")), btn("X", T("download")), selectBtn(T("multi_select")))

	res, err := gaba.List(opts)
	if e.QuitRequested() {
		return Exit, nil
	}
	if gaba.IsCancelled(err) {
		return e.browserBack(dir), nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Selected) > 0 {
		first := res.Selected[0]
		s.cursor[dir] = listPos{selected: first, visibleStart: max(0, first-res.VisiblePosition)}
	}

	switch res.Action {
	case gaba.ListActionSecondaryTriggered: // Y: refresh
		return Nav{To: ScreenBrowser, Input: dir}, nil
	case gaba.ListActionTriggered: // X: download focused or selected items
		if len(res.Selected) == 0 {
			info(T("nothing_selected"))
			return Nav{To: ScreenBrowser, Input: dir}, nil
		}
		sort.Ints(res.Selected)
		sel := &Selection{Dir: dir}
		for _, i := range res.Selected {
			sel.Entries = append(sel.Entries, entries[i])
		}
		return Nav{To: ScreenTarget, Input: sel}, nil
	}

	if len(res.Selected) == 0 {
		return Nav{To: ScreenBrowser, Input: dir}, nil
	}
	ent := entries[res.Selected[0]]
	path := remote.Join(dir, ent.Name)
	switch {
	case ent.IsDir:
		return Nav{To: ScreenBrowser, Input: path}, nil
	case ent.IsLink:
		// Backends resolve links to folders; what is left is a file whose
		// listed size is the link's own length, not the target's.
		ent.Size = -1
	}
	return Nav{To: ScreenTarget, Input: &Selection{Dir: dir, Entries: []remote.Entry{ent}}}, nil
}

func (e *Env) browserBack(dir string) Nav {
	if dir == "/" {
		return Nav{To: ScreenServers}
	}
	return Nav{To: ScreenBrowser, Input: remote.Parent(dir)}
}

// listRemote lists a folder, reconnecting once if the connection died
// (idle NAS timeouts, Wi-Fi power saving).
func (e *Env) listRemote(dir string) ([]remote.Entry, error) {
	s := e.session
	var entries []remote.Entry
	list := func() error {
		return e.runTask(T("loading"), true, func(ctx context.Context) error {
			var err error
			entries, err = s.Backend.List(ctx, dir)
			return err
		})
	}
	err := list()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) && !e.QuitRequested() {
		if rerr := e.reconnect(); rerr == nil {
			err = list()
		}
	}
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, ent := range entries {
		if !e.Settings.ShowHidden && strings.HasPrefix(ent.Name, ".") {
			continue
		}
		out = append(out, ent)
	}
	remote.SortEntries(out)
	return out, nil
}

func (e *Env) reconnect() error {
	s := e.session
	_ = s.Backend.Close()
	return e.runTask(T("reconnecting"), true, func(ctx context.Context) error {
		be, err := s.NewBackend(ctx)
		if err != nil {
			return err
		}
		s.Backend = be
		return nil
	})
}

func entryLabel(ent remote.Entry) string {
	switch {
	case ent.IsDir:
		return iconFolder + "  " + ent.Name
	case ent.IsLink:
		return iconLink + "  " + ent.Name
	case archive.IsArchive(ent.Name):
		return fmt.Sprintf("%s  %s  · %s", iconArchive, ent.Name, humanize.Bytes(ent.Size))
	default:
		return fmt.Sprintf("%s  %s  · %s", iconFile, ent.Name, humanize.Bytes(ent.Size))
	}
}
