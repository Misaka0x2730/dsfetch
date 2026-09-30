package ui

import (
	"fmt"
	"runtime"
	"time"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"

	"dsfetch/internal/i18n"
	"dsfetch/internal/store"
	"dsfetch/internal/version"
)

func choiceOptions(values []string, labels []string, current string) ([]gaba.Option, int) {
	opts := make([]gaba.Option, len(values))
	sel := 0
	for i, v := range values {
		opts[i] = gaba.Option{DisplayName: labels[i], Value: v}
		if v == current {
			sel = i
		}
	}
	return opts, sel
}

func optValue(it gaba.ItemWithOptions) string {
	s, _ := it.Options[it.SelectedOption].Value.(string)
	return s
}

// SettingsScreen edits preferences; B saves and goes back.
func (e *Env) SettingsScreen(any) (any, error) {
	s := e.Settings
	askValues := []string{store.Ask, store.Always, store.Never}
	askLabels := []string{T("ask"), T("always"), T("never")}

	langNames := []string{T("lang_auto")}
	for _, code := range i18n.Languages[1:] {
		langNames = append(langNames, i18n.Name(code))
	}
	langOpts, langSel := choiceOptions(i18n.Languages, langNames, s.Language)
	unpackOpts, unpackSel := choiceOptions(askValues, askLabels, s.ExtractMode)
	deleteOpts, deleteSel := choiceOptions(askValues, askLabels, s.DeleteArchive)

	items := []gaba.ItemWithOptions{
		{Item: gaba.MenuItem{Text: T("s_language")}, Options: langOpts, SelectedOption: langSel},
		{Item: gaba.MenuItem{Text: T("s_unpack")}, Options: unpackOpts, SelectedOption: unpackSel},
		{Item: gaba.MenuItem{Text: T("s_delete_archive")}, Options: deleteOpts, SelectedOption: deleteSel},
		{Item: gaba.MenuItem{Text: T("s_flatten")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(s.FlattenSingleDir)},
		{Item: gaba.MenuItem{Text: T("s_show_hidden")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(s.ShowHidden)},
	}
	lidRow := -1
	if e.Platform.CanSleepOnLid() {
		lidRow = len(items)
		items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("s_lid_sleep")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(s.LidSleep)})
	}
	idleRow := -1
	if e.Platform.CanSleepWhenIdle() {
		idleRow = len(items)
		items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("s_idle_sleep")}, Options: yesNoOptions(nil), SelectedOption: boolIndex(s.IdleSleep)})
	}
	engineRow := -1
	if e.SevenZipBinary != "" {
		engineRow = len(items)
		opts, sel := choiceOptions([]string{store.SevenZipAuto, store.SevenZipBuiltin, store.SevenZipCLI}, []string{T("engine_auto"), T("engine_builtin"), "7zz"}, s.SevenZipEngine)
		items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("s_7z_engine")}, Options: opts, SelectedOption: sel})
	}
	aboutRow := len(items)
	items = append(items, gaba.ItemWithOptions{Item: gaba.MenuItem{Text: T("about")}, Options: []gaba.Option{{Type: gaba.OptionTypeClickable}}})

	settings := gaba.OptionListSettings{
		FooterHelpItems:  footer(btn("B", T("save_back")), btn("A", T("change"))),
		ListPickerButton: constants.VirtualButtonA,
		HelpExitText:     T("help_exit"),
	}
	for {
		res, err := gaba.OptionsList(T("settings"), settings, items)
		if e.QuitRequested() {
			return Exit, nil
		}
		if err != nil && !gaba.IsCancelled(err) {
			return nil, err
		}
		if err == nil && res.Selected == aboutRow {
			settings.InitialSelectedIndex = aboutRow
			info(e.aboutText())
			continue
		}
		break
	}

	// gabagool edits items in place, so the values are there even after B.
	s.Language = optValue(items[0])
	s.ExtractMode = optValue(items[1])
	s.DeleteArchive = optValue(items[2])
	s.FlattenSingleDir = optBool(items[3])
	s.ShowHidden = optBool(items[4])
	if lidRow >= 0 {
		s.LidSleep = optBool(items[lidRow])
	}
	if idleRow >= 0 {
		s.IdleSleep = optBool(items[idleRow])
	}
	if engineRow >= 0 {
		s.SevenZipEngine = optValue(items[engineRow])
	}
	e.SaveSettings(s)
	return Nav{To: ScreenServers}, nil
}

func (e *Env) aboutText() string {
	build := fmt.Sprintf("%s/%s, %s", runtime.GOOS, runtime.GOARCH, version.GitCommit)
	if t, err := time.Parse(time.RFC3339, version.BuildDate); err == nil { // "unknown" in dev builds
		build += ", " + t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf("DSFetch %s\n%s\n%s\n\n%s\n\n%s: %s\n%s", version.Version, T("about_tagline"),
		version.Homepage, T("about_licenses"), T("data_folder"), e.DataDir, build)
}
