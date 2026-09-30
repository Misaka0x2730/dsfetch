package ui

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/platform"
)

type folderRowKind int

const (
	folderChoose folderRowKind = iota
	folderUp
	folderNew
	folderSub
)

type folderRow struct {
	kind folderRowKind
	name string
}

// browseFolder lets the user pick a folder on a card by walking the tree
// ("..", "new folder", "choose this folder", subfolders). rel is the start
// folder relative to the card root; if it does not exist yet the nearest
// existing parent is shown. The cursor starts on the first subfolder, or
// after ".." on the folder just left. START picks the folder shown, B
// cancels.
func (e *Env) browseFolder(card platform.Card, rel string) (string, bool) {
	cur := cleanRel(rel)
	for cur != "" && !dirExists(filepath.Join(card.Path, filepath.FromSlash(cur))) {
		cur = parentRel(cur)
	}
	visited := map[string]listPos{}
	from := "" // the subfolder just left with ".."

	for {
		items := folderItems(cur != "", subfolders(filepath.Join(card.Path, filepath.FromSlash(cur))))
		saved, seen := visited[cur]
		pos := folderCursor(items, saved, seen, from)
		from = ""

		opts := gaba.DefaultListOptions(card.Label+":/"+cur, items)
		opts.UseSmallTitle = true
		opts.SelectedIndex, opts.VisibleStartIndex = pos.selected, pos.visibleStart
		opts.ActionButton = constants.VirtualButtonStart
		opts.MultiSelectConfirmButton = constants.VirtualButtonUnassigned
		opts.FooterHelpItems = footer(btn("B", T("cancel")), btn("A", T("open")), startBtn(T("select")))
		res, err := gaba.List(opts)
		if err != nil || e.QuitRequested() {
			return "", false
		}
		if res.Action == gaba.ListActionTriggered { // START
			return cur, true
		}
		if len(res.Selected) == 0 {
			return "", false
		}
		idx := res.Selected[0]
		visited[cur] = listPos{selected: idx, visibleStart: max(0, idx-res.VisiblePosition)}
		row, ok := items[idx].Metadata.(folderRow)
		if !ok {
			continue // a divider
		}
		switch row.kind {
		case folderChoose:
			return cur, true
		case folderUp:
			from = path.Base(cur)
			cur = parentRel(cur)
		case folderSub:
			cur = path.Join(cur, row.name)
		case folderNew:
			name, ok := e.askFolderName()
			if !ok {
				continue
			}
			sub := path.Join(cur, name)
			if err := os.MkdirAll(filepath.Join(card.Path, filepath.FromSlash(sub)), 0o755); err != nil {
				info(errorMessage(T("err_mkdir", name), err))
				continue
			}
			cur = sub
		}
	}
}

// folderItems builds the browser rows: the actions ("..", "new folder",
// "choose this folder" next to the subfolders), a divider that always closes
// them (it says so when there are no subfolders), then subfolders.
func folderItems(hasParent bool, subs []string) []gaba.MenuItem {
	var items []gaba.MenuItem
	if hasParent {
		items = append(items, gaba.MenuItem{Text: iconUp + "  ..", Metadata: folderRow{kind: folderUp}})
	}
	items = append(items,
		gaba.MenuItem{Text: "+  " + T("new_folder"), Metadata: folderRow{kind: folderNew}},
		gaba.MenuItem{Text: "✓  " + T("choose_this_folder"), Metadata: folderRow{kind: folderChoose}},
	)
	divider := T("folders_header")
	if len(subs) == 0 {
		divider = T("no_subfolders")
	}
	items = append(items, gaba.MenuItem{Text: divider, Separator: true})
	for _, name := range subs {
		items = append(items, gaba.MenuItem{Text: iconFolder + "  " + name, Metadata: folderRow{kind: folderSub, name: name}})
	}
	return items
}

// folderCursor picks the starting row: the subfolder just left with ".."
// (from), else the row of the last visit, else the first subfolder, else
// "choose this folder".
func folderCursor(items []gaba.MenuItem, saved listPos, seen bool, from string) listPos {
	first, choose := -1, 0
	for i, it := range items {
		row, ok := it.Metadata.(folderRow)
		if ok && row.kind == folderChoose {
			choose = i
		}
		if !ok || row.kind != folderSub {
			continue
		}
		if first < 0 {
			first = i
		}
		if from != "" && row.name == from {
			if seen && saved.selected == i {
				return saved
			}
			return listPos{selected: i} // the list scrolls it into view
		}
	}
	if seen && saved.selected < len(items) {
		return saved
	}
	if first >= 0 {
		return listPos{selected: first}
	}
	return listPos{selected: choose}
}

// askFolderName reads a new folder name with the on-screen keyboard.
func (e *Env) askFolderName() (string, bool) {
	res, err := gaba.Keyboard("", T("help_exit"))
	if err != nil || e.QuitRequested() {
		return "", false
	}
	name := platform.SafeFileName(strings.TrimSpace(res.Text))
	if name == "_" || name == "" {
		return "", false
	}
	return name, true
}

func subfolders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() && !strings.HasPrefix(n, ".") && n != "System Volume Information" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func cleanRel(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	return strings.Trim(path.Clean("/"+rel), "/")
}

func parentRel(rel string) string {
	p := path.Dir(rel)
	if p == "." || p == "/" {
		return ""
	}
	return p
}
