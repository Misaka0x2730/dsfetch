package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
	"dsfetch/internal/remote"
	"dsfetch/internal/store"
)

// QueueJob is a confirmed download.
type QueueJob struct {
	Sel      *Selection
	LocalDir string
	Unpack   bool
}

// targetForm is the card + folder part of a destination picker, shared by
// the download target screen and local archive extraction. The folder row
// opens a folder browser (browseFolder).
type targetForm struct {
	cards []platform.Card
	items []gaba.ItemWithOptions
	rel   string // folder relative to the card root
	// onFolder runs when the folder changes.
	onFolder func(rel string)
}

const (
	rowCard = iota
	rowFolder
)

// newTargetForm preselects prefCard/prefRel (a remembered target) or the
// suggested system's folder, else Roms.
func (e *Env) newTargetForm(prefCard, prefRel string, sug *platform.System) *targetForm {
	cards := e.Platform.AvailableCards()
	f := &targetForm{cards: cards}

	cardOpts := make([]gaba.Option, len(cards))
	free := make([]uint64, len(cards))
	cardSel := -1
	for i, c := range cards {
		label := c.Label
		if n, ok := e.Platform.FreeSpace(c); ok {
			free[i] = n
			label = fmt.Sprintf("%s · %s %s", c.Label, humanize.Bytes(int64(n)), T("free"))
		}
		cardOpts[i] = gaba.Option{DisplayName: label, Value: i}
		if c.ID == prefCard {
			cardSel = i
		}
	}
	if cardSel < 0 {
		// Nothing remembered: the roomiest card, preferring cards that
		// already have the system's folder (on the RG DS Plus TF1 holds a
		// 3 GB games partition, TF2 is the big games card).
		best := -1
		for pass := 0; pass < 2 && best < 0; pass++ {
			for i, c := range cards {
				if pass == 0 && (sug == nil || !dirExists(filepath.Join(e.Platform.RomsPath(c), e.Platform.ExistingDirFor(c, *sug)))) {
					continue
				}
				if best < 0 || free[i] > free[best] {
					best = i
				}
			}
		}
		cardSel = max(best, 0)
	}

	f.rel = e.Platform.RomsDir
	switch {
	case prefRel != "":
		f.rel = cleanRel(prefRel)
	case sug != nil && len(cards) > 0:
		f.rel = path.Join(e.Platform.RomsDir, e.Platform.ExistingDirFor(cards[cardSel], *sug))
	}

	f.items = []gaba.ItemWithOptions{
		{Item: gaba.MenuItem{Text: T("f_card")}, Options: cardOpts, SelectedOption: cardSel},
		{Item: gaba.MenuItem{Text: T("f_folder")}, Options: []gaba.Option{{DisplayName: folderLabel(f.rel), Value: f.rel, Type: gaba.OptionTypeClickable}}},
	}
	return f
}

func folderLabel(rel string) string {
	return "/" + rel
}

// card returns the selected card.
func (f *targetForm) card() platform.Card {
	ci, _ := f.items[rowCard].Options[f.items[rowCard].SelectedOption].Value.(int)
	return f.cards[ci]
}

// browse opens the folder browser on the selected card.
func (f *targetForm) browse(e *Env) {
	rel, ok := e.browseFolder(f.card(), f.rel)
	if !ok {
		return
	}
	f.rel = rel
	f.items[rowFolder].Options[0].DisplayName = folderLabel(rel)
	f.items[rowFolder].Options[0].Value = rel
	if f.onFolder != nil {
		f.onFolder(rel)
	}
}

// choice returns the selected card and folder (relative to the card root).
func (f *targetForm) choice() (platform.Card, string, error) {
	parts := strings.Split(cleanRel(f.rel), "/")
	for i, p := range parts {
		if p == ".." {
			return f.card(), "", errors.New(T("err_folder_invalid"))
		}
		if p != "" {
			parts[i] = platform.SafeFileName(p)
		}
	}
	return f.card(), strings.Join(parts, "/"), nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func parseTarget(v string) (cardID, rel string) {
	cardID, rel, _ = strings.Cut(v, ":")
	return cardID, rel
}

// Target asks where to put a selection (input: *Selection).
func (e *Env) Target(in any) (any, error) {
	sel, _ := in.(*Selection)
	s := e.session
	if s == nil || sel == nil || len(sel.Entries) == 0 {
		return Nav{To: ScreenServers}, nil
	}
	if len(e.Platform.AvailableCards()) == 0 {
		info(T("err_no_cards"))
		return Nav{To: ScreenBrowser, Input: sel.Dir}, nil
	}

	var sug *platform.System
	hasArchives := false
	for _, ent := range sel.Entries {
		name, dir := ent.Name, sel.Dir
		if ent.IsDir {
			name, dir = "", remote.Join(sel.Dir, ent.Name)
			hasArchives = true // folders may contain archives
		} else if archive.IsArchive(ent.Name) {
			hasArchives = true
		}
		if sug == nil {
			if sys, ok := e.Platform.SuggestSystem(name, dir); ok {
				sys := sys
				sug = &sys
			}
		}
	}
	prefCard, prefRel := parseTarget(s.Server.LastTargets[sel.Dir])
	form := e.newTargetForm(prefCard, prefRel, sug)
	items := form.items

	unpackRow := -1
	if hasArchives && e.Settings.ExtractMode == store.Ask {
		unpackRow = len(items)
		items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("f_unpack")}, Options: yesNoOptions(nil),
			SelectedOption: boolIndex(!e.Platform.KeepsArchives(form.rel))})
		form.onFolder = func(rel string) {
			items[unpackRow].SelectedOption = boolIndex(!e.Platform.KeepsArchives(rel))
		}
	}
	goRow := len(items)
	items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("download")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}})
	form.items = items

	title := T("download_many", len(sel.Entries))
	if len(sel.Entries) == 1 {
		title = T("download_one", sel.Entries[0].Name)
	}
	settings := gaba.OptionListSettings{
		FooterHelpItems:  footer(btn("B", T("back")), btn("A", T("change")), startBtn(T("download"))),
		UseSmallTitle:    true,
		ListPickerButton: constants.VirtualButtonA,
		HelpExitText:     T("help_exit"),
	}
	for {
		res, err := gaba.OptionsList(title, settings, items)
		if e.QuitRequested() {
			return Exit, nil
		}
		if gaba.IsCancelled(err) {
			return Nav{To: ScreenBrowser, Input: sel.Dir}, nil
		}
		if err != nil {
			return nil, err
		}
		settings.InitialSelectedIndex = res.Selected
		if res.Action == gaba.ListActionSelected && res.Selected == rowFolder {
			form.browse(e)
			continue
		}
		if res.Action != gaba.ListActionConfirmed && res.Selected != goRow {
			continue
		}
		card, rel, err := form.choice()
		if err != nil {
			info(err.Error())
			continue
		}
		unpack := e.Settings.ExtractMode == store.Always && !e.Platform.KeepsArchives(rel)
		if unpackRow >= 0 {
			unpack = optBool(items[unpackRow])
		}
		target := card.ID + ":" + rel
		if s.Server.LastTargets[sel.Dir] != target {
			if s.Server.LastTargets == nil {
				s.Server.LastTargets = map[string]string{}
			}
			s.Server.LastTargets[sel.Dir] = target
			if err := e.Store.Update(s.Server.ID, func(sv *store.Server) {
				if sv.LastTargets == nil {
					sv.LastTargets = map[string]string{}
				}
				sv.LastTargets[sel.Dir] = target
			}); err != nil {
				slog.Warn("remember target", "err", err)
			}
		}
		return Nav{To: ScreenQueue, Input: &QueueJob{
			Sel:      sel,
			LocalDir: filepath.Join(card.Path, filepath.FromSlash(rel)),
			Unpack:   unpack,
		}}, nil
	}
}
