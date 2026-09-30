package ui

import (
	"testing"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"

	"dsfetch/internal/i18n"
)

func TestMain(m *testing.M) {
	if err := i18n.Init("en"); err != nil {
		panic(err)
	}
	m.Run()
}

func TestFolderItemsAlwaysDivideActions(t *testing.T) {
	items := folderItems(true, nil)
	last := items[len(items)-1]
	if !last.Separator || last.Text != T("no_subfolders") {
		t.Fatalf("empty folder: last row = %+v, want the no-subfolders divider", last)
	}
	items = folderItems(false, []string{"GBA", "NDS"})
	if len(items) != 5 || !items[2].Separator || items[2].Text != T("folders_header") {
		t.Fatalf("card root rows = %+v", items)
	}
}

func TestFolderItemsActionOrder(t *testing.T) {
	want := []folderRowKind{folderUp, folderNew, folderChoose}
	items := folderItems(true, nil)
	for i, kind := range want {
		if row, _ := items[i].Metadata.(folderRow); row.kind != kind {
			t.Errorf("row %d: kind %d, want %d", i, row.kind, kind)
		}
	}
	root := folderItems(false, nil) // no ".." at the card root
	if row, _ := root[0].Metadata.(folderRow); row.kind != folderNew {
		t.Errorf("card root starts with kind %d, want new folder", row.kind)
	}
}

func TestFolderCursor(t *testing.T) {
	subs := folderItems(true, []string{"GBA", "NDS", "SFC"}) // .., new, choose, divider, GBA=4, NDS=5, SFC=6
	empty := folderItems(true, nil)                          // .., new, choose=2, divider
	cases := []struct {
		name  string
		items []gaba.MenuItem
		saved listPos
		seen  bool
		from  string
		want  listPos
	}{
		{"first visit: first subfolder", subs, listPos{}, false, "", listPos{selected: 4}},
		{"no subfolders: choose this folder", empty, listPos{}, false, "", listPos{selected: 2}},
		{"card root, no subfolders", folderItems(false, nil), listPos{}, false, "", listPos{selected: 1}},
		{"revisit: last position", subs, listPos{selected: 1, visibleStart: 0}, true, "", listPos{selected: 1}},
		{"up: the folder just left", subs, listPos{}, false, "SFC", listPos{selected: 6}},
		{"up: keeps the saved window", subs, listPos{selected: 5, visibleStart: 2}, true, "NDS", listPos{selected: 5, visibleStart: 2}},
		{"up: left folder gone", subs, listPos{}, false, "PSP", listPos{selected: 4}},
		{"stale position", empty, listPos{selected: 9}, true, "", listPos{selected: 2}},
	}
	for _, c := range cases {
		if got := folderCursor(c.items, c.saved, c.seen, c.from); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
