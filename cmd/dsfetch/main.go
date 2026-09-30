// Command dsfetch is an FTP/SFTP/SMB client for the Anbernic RG DS Plus.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"

	"dsfetch/internal/app"
	"dsfetch/internal/logging"
	"dsfetch/internal/version"
)

func main() {
	var (
		dev         = flag.Bool("dev", false, "development mode: 1024x768 window, logs to stdout")
		root        = flag.String("root", "", "filesystem prefix for card paths, e.g. ./devroot")
		screen      = flag.Int("screen", -1, "SDL display index to show the UI on (-1 = the system's main screen)")
		rotate      = flag.Int("rotate", 0, "clockwise display rotation: 0, 90, 180 or 270")
		dataDir     = flag.String("data", "", "data directory (default: <binary dir>/data, ./devdata with -dev)")
		showVersion = flag.Bool("version", false, "print version and exit")
		bench       = flag.String("bench", "", "unpack this .7z or .rar with each engine (a .zip with the built-in one), print time and memory, exit")
		benchPass   = flag.String("bench-password", "", "password for -bench")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("dsfetch", version.String())
		return
	}

	// 1 GB of RAM is shared with the frontend; keep the Go heap well below it
	// (also for -bench, so that it measures what the app does).
	debug.SetMemoryLimit(300 << 20)

	if *bench != "" {
		os.Exit(runBench(*bench, *benchPass))
	}

	if *dev {
		// gabagool reads this to open a 1024x768 desktop window.
		os.Setenv("ENVIRONMENT", "DEV")
	}

	data := resolveDataDir(*dataDir, *dev)
	if err := os.MkdirAll(data, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create data dir:", err)
		os.Exit(1)
	}

	logCloser, err := logging.Setup(*dev, filepath.Join(data, "dsfetch.log"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "log setup:", err)
	}
	defer logCloser.Close()

	slog.Info("dsfetch starting", "version", version.String(), "dev", *dev, "root", *root, "data", data)

	err = app.Run(app.Options{
		Dev:     *dev,
		Root:    *root,
		DataDir: data,
		Screen:  *screen,
		Rotate:  *rotate,
	})
	if err != nil {
		slog.Error("dsfetch exited with error", "err", err)
		logCloser.Close()
		os.Exit(1)
	}
	slog.Info("dsfetch exited")
}

func resolveDataDir(flagValue string, dev bool) string {
	if flagValue != "" {
		abs, err := filepath.Abs(flagValue)
		if err == nil {
			return abs
		}
		return flagValue
	}
	if dev {
		abs, err := filepath.Abs("devdata")
		if err == nil {
			return abs
		}
		return "devdata"
	}
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "data")
}
