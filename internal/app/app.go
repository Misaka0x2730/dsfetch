// Package app wires dependencies together and runs the screen router.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/router"

	"dsfetch/internal/archive"
	"dsfetch/internal/i18n"
	"dsfetch/internal/platform"
	"dsfetch/internal/store"
	"dsfetch/internal/ui"
)

// Options come from command-line flags.
type Options struct {
	Dev     bool
	Root    string
	DataDir string
	Screen  int
	Rotate  int
}

// Run initialises the UI and blocks until the user quits.
func Run(opts Options) error {
	plat, err := platform.Load(platform.Options{Root: opts.Root, DataDir: opts.DataDir, Dev: opts.Dev})
	if err != nil {
		return err
	}
	platform.SetDevOverrides(opts.Dev)
	for _, c := range plat.AvailableCards() {
		slog.Info("card", "id", c.ID, "path", c.Path)
		// Left behind by an unpack that was cut short (power off).
		if found, err := archive.RemoveStaleTemp(c.Path); found {
			slog.Info("removed an interrupted unpack", "card", c.ID, "err", err)
		}
	}

	settings, err := store.LoadSettings(opts.DataDir)
	if err != nil {
		slog.Warn("settings unreadable, using defaults", "err", err)
	}
	// Saved passwords are bound to this console (nil ID on a computer).
	deviceID := plat.DeviceID()
	slog.Info("device ID for saved passwords", "present", deviceID != nil)
	st, err := store.Open(opts.DataDir, deviceID)
	if err != nil {
		return fmt.Errorf("open server list: %w", err)
	}
	if lang := plat.SystemLanguage(); lang != "" {
		slog.Info("system language", "lang", lang)
		i18n.SetSystemLanguage(lang)
	}
	if err := i18n.Init(settings.Language); err != nil {
		return fmt.Errorf("i18n: %w", err)
	}

	env := &ui.Env{
		Platform:       plat,
		Store:          st,
		Settings:       settings,
		DataDir:        opts.DataDir,
		SevenZipBinary: FindSevenZip(opts.Dev),
		QuitRequested:  QuitRequested,
	}
	if env.SevenZipBinary != "" {
		slog.Info("7zz available", "path", env.SevenZipBinary)
	}

	// Follow the system "swap screens" setting unless -screen was given.
	if opts.Screen < 0 && plat.ScreensSwapped() {
		slog.Info("screens swapped in system settings: using display 1")
		opts.Screen = 1
	}
	onActivity = plat.Touch // button presses reset the idle sleep timer
	initUI(opts)
	defer gaba.Close()
	defer stopQuitRepeat()
	defer env.Shutdown()
	ui.ApplyLanguage()

	// Suspend on lid close and after the system sleep timer, like the system
	// menu (not while working).
	plat.SetLidSleep(settings.LidSleep)
	plat.SetIdleSleep(settings.IdleSleep)
	sleepCtx, stopSleep := context.WithCancel(context.Background())
	defer stopSleep()
	go plat.WatchSleep(sleepCtx)

	// Damaged data files were renamed to .bad above: say so now that there
	// is a screen.
	ui.ReportSetAside(append(plat.SetAside, st.SetAside()...))

	r := router.New()
	r.Register(ui.ScreenServers, env.Servers)
	r.Register(ui.ScreenServerEdit, env.ServerEdit)
	r.Register(ui.ScreenConnect, env.Connect)
	r.Register(ui.ScreenBrowser, env.Browser)
	r.Register(ui.ScreenTarget, env.Target)
	r.Register(ui.ScreenQueue, env.Queue)
	r.Register(ui.ScreenLocal, env.Local)
	r.Register(ui.ScreenSettings, env.SettingsScreen)
	r.OnTransition(func(from router.Screen, result any, _ *router.Stack) (router.Screen, any) {
		if QuitRequested() {
			return router.ScreenExit, nil
		}
		nav, ok := result.(ui.Nav)
		if !ok {
			slog.Error("screen returned no navigation", "screen", from)
			return ui.ScreenServers, nil
		}
		return nav.To, nav.Input
	})
	return r.Run(ui.ScreenServers, nil)
}

// FindSevenZip looks for the optional 7zz next to the binary (or on PATH
// in dev mode, e.g. brew's sevenzip) and checks that it actually runs: the
// official build needs libstdc++ on the device.
func FindSevenZip(dev bool) string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "7zz"), filepath.Join(filepath.Dir(exe), "bin", "7zz"))
	}
	if dev {
		if p, err := exec.LookPath("7zz"); err == nil {
			candidates = append(candidates, p)
		}
	}
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = exec.CommandContext(ctx, p).Run()
		cancel()
		if err != nil {
			slog.Warn("7zz found but does not run", "path", p, "err", err)
			continue
		}
		return p
	}
	return ""
}

func initUI(opts Options) {
	// Without focus SDL drops joystick events; a Wayland compositor may not
	// focus a freshly launched window, and this app is gamepad-only.
	if os.Getenv("SDL_JOYSTICK_ALLOW_BACKGROUND_EVENTS") == "" {
		os.Setenv("SDL_JOYSTICK_ALLOW_BACKGROUND_EVENTS", "1")
	}
	autopilot := autopilotEnabled(opts.Dev)
	if autopilot {
		prepareAutopilot()
	}
	if b, err := os.ReadFile(filepath.Join(opts.DataDir, "input_mapping.json")); err == nil {
		gaba.SetInputMappingBytes(b)
		slog.Info("custom input mapping loaded")
	}
	gaba.Init(gaba.Options{
		WindowTitle:        "DSFetch",
		LogPath:            filepath.Join(opts.DataDir, "gabagool.log"),
		DisplayOrientation: orientation(opts.Rotate),
	})
	installQuitWatch()
	installQuitChord()
	logDisplays()
	if !opts.Dev {
		placeWindow(opts.Screen, opts.Rotate != 0)
	} else if opts.Screen >= 0 {
		moveToDisplay(opts.Screen)
	}
	if autopilot {
		startAutopilot()
	}
}

func orientation(deg int) gaba.DisplayOrientation {
	switch deg {
	case 90:
		return gaba.OrientationRotate90
	case 180:
		return gaba.OrientationRotate180
	case 270:
		return gaba.OrientationRotate270
	default:
		return gaba.OrientationNormal
	}
}
