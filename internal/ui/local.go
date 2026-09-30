package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
)

type localEntry struct {
	name  string
	isDir bool
	size  int64
}

// Local browses the memory cards for archives (input: folder, "" = cards).
func (e *Env) Local(in any) (any, error) {
	dir, _ := in.(string)
	cards := e.Platform.AvailableCards()
	if dir == "" {
		return e.localCards(cards)
	}
	card, ok := cardFor(cards, dir)
	if !ok {
		return Nav{To: ScreenLocal}, nil
	}

	entries, err := readLocal(dir)
	if err != nil {
		info(errorMessage(T("err_list", dir), err))
		return e.localBack(card, dir), nil
	}
	items := make([]gaba.MenuItem, len(entries))
	for i, ent := range entries {
		if ent.isDir {
			items[i] = gaba.MenuItem{Text: iconFolder + "  " + ent.name}
		} else {
			items[i] = gaba.MenuItem{Text: fmt.Sprintf("%s  %s  · %s", iconArchive, ent.name, humanize.Bytes(ent.size))}
		}
	}
	rel, _ := filepath.Rel(card.Path, dir)
	title := card.Label
	if rel != "." {
		title += ":/" + filepath.ToSlash(rel)
	}
	opts := gaba.DefaultListOptions(title, items)
	opts.UseSmallTitle = true
	opts.EmptyMessage = T("no_archives_here")
	if e.localCursor == nil {
		e.localCursor = map[string]listPos{}
	}
	if pos, ok := e.localCursor[dir]; ok {
		opts.SelectedIndex, opts.VisibleStartIndex = pos.selected, pos.visibleStart
	}
	opts.FooterHelpItems = footer(btn("B", T("back")), btn("A", T("open")))

	res, err := gaba.List(opts)
	if e.QuitRequested() {
		return Exit, nil
	}
	if gaba.IsCancelled(err) {
		return e.localBack(card, dir), nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Selected) == 0 {
		return Nav{To: ScreenLocal, Input: dir}, nil
	}
	idx := res.Selected[0]
	e.localCursor[dir] = listPos{selected: idx, visibleStart: max(0, idx-res.VisiblePosition)}
	ent := entries[idx]
	path := filepath.Join(dir, ent.name)
	if ent.isDir {
		return Nav{To: ScreenLocal, Input: path}, nil
	}

	choice, ok := choose(ent.name, []gaba.SelectionOption{
		{DisplayName: T("unpack_here"), Value: 0},
		{DisplayName: T("unpack_to"), Value: 1},
		{DisplayName: T("delete_archive"), Value: 2},
	})
	if !ok || e.QuitRequested() {
		return Nav{To: ScreenLocal, Input: dir}, nil
	}
	switch choice {
	case 0:
		_, _ = e.extractFlow(path, dir)
	case 1:
		if dst, ok := e.pickLocalTarget(card, dir, ent.name); ok {
			_, _ = e.extractFlow(path, dst)
		}
	case 2:
		if confirm(T("delete_archive_q", ent.name), T("delete"), T("cancel")) {
			for _, v := range archive.Volumes(path) {
				if err := os.Remove(v); err != nil {
					info(errorMessage(T("err_delete"), err))
					break
				}
			}
		}
	}
	if e.QuitRequested() {
		return Exit, nil
	}
	return Nav{To: ScreenLocal, Input: dir}, nil
}

func (e *Env) localCards(cards []platform.Card) (any, error) {
	if len(cards) == 0 {
		info(T("err_no_cards"))
		return Nav{To: ScreenServers}, nil
	}
	items := make([]gaba.MenuItem, len(cards))
	for i, c := range cards {
		label := fmt.Sprintf("%s  %s  · %s", iconCard, c.Label, c.Path)
		if n, ok := e.Platform.FreeSpace(c); ok {
			label += fmt.Sprintf("  · %s %s", humanize.Bytes(int64(n)), T("free"))
		}
		items[i] = gaba.MenuItem{Text: label}
	}
	opts := gaba.DefaultListOptions(T("local_archives"), items)
	opts.FooterHelpItems = footer(btn("B", T("back")), btn("A", T("open")))
	res, err := gaba.List(opts)
	if e.QuitRequested() {
		return Exit, nil
	}
	if gaba.IsCancelled(err) {
		return Nav{To: ScreenServers}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Selected) == 0 {
		return Nav{To: ScreenLocal}, nil
	}
	return Nav{To: ScreenLocal, Input: cards[res.Selected[0]].Path}, nil
}

func (e *Env) localBack(card platform.Card, dir string) Nav {
	if filepath.Clean(dir) == filepath.Clean(card.Path) {
		return Nav{To: ScreenLocal}
	}
	return Nav{To: ScreenLocal, Input: filepath.Dir(dir)}
}

func cardFor(cards []platform.Card, dir string) (platform.Card, bool) {
	for _, c := range cards {
		rel, err := filepath.Rel(c.Path, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return c, true
		}
	}
	return platform.Card{}, false
}

// readLocal lists folders and archives (first volumes only).
func readLocal(dir string) ([]localEntry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []localEntry
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if de.IsDir() {
			out = append(out, localEntry{name: name, isDir: true})
			continue
		}
		if !archive.IsArchive(name) {
			continue
		}
		var size int64
		for _, v := range archive.Volumes(filepath.Join(dir, name)) {
			if fi, err := os.Stat(v); err == nil {
				size += fi.Size()
			}
		}
		out = append(out, localEntry{name: name, size: size})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].isDir != out[j].isDir {
			return out[i].isDir
		}
		return strings.ToLower(out[i].name) < strings.ToLower(out[j].name)
	})
	return out, nil
}

// pickLocalTarget asks for a destination folder for a local archive.
func (e *Env) pickLocalTarget(card platform.Card, dir, archiveName string) (string, bool) {
	rel, _ := filepath.Rel(card.Path, dir)
	var sug *platform.System
	if s, ok := e.Platform.SuggestSystem(archiveName, ""); ok {
		sug = &s
	}
	pref := ""
	if rel != "." && sug == nil {
		pref = filepath.ToSlash(rel)
	}
	form := e.newTargetForm(card.ID, pref, sug)
	goRow := len(form.items)
	form.items = append(form.items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("unpack")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}})
	settings := gaba.OptionListSettings{
		FooterHelpItems:  footer(btn("B", T("back")), btn("A", T("change")), startBtn(T("unpack"))),
		ListPickerButton: constants.VirtualButtonA,
		HelpExitText:     T("help_exit"),
	}
	for {
		res, err := gaba.OptionsList(T("unpack_where", archiveName), settings, form.items)
		if err != nil || e.QuitRequested() {
			return "", false
		}
		settings.InitialSelectedIndex = res.Selected
		if res.Action == gaba.ListActionSelected && res.Selected == rowFolder {
			form.browse(e)
			continue
		}
		if res.Action != gaba.ListActionConfirmed && res.Selected != goRow {
			continue
		}
		c, r, err := form.choice()
		if err != nil {
			info(err.Error())
			continue
		}
		return filepath.Join(c.Path, filepath.FromSlash(r)), true
	}
}
