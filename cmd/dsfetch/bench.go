package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"dsfetch/internal/app"
	"dsfetch/internal/archive"
	"dsfetch/internal/humanize"
	"dsfetch/internal/platform"
)

// runBench is the Stage 4 measurement: unpack one archive (.7z or .rar) with
// each engine and report time and peak memory (a .zip is always unpacked by
// the built-in engine). Run it on the console over SSH:
//
//	cd /mnt/sdcard/Ports/DSFetch && LD_LIBRARY_PATH=./lib ./dsfetch -bench /mnt/sdcard/iso.7z
func runBench(path, password string) int {
	info, err := archive.Inspect(path, archive.Options{Password: password})
	if err != nil {
		fmt.Fprintln(os.Stderr, "inspect:", err)
		return 1
	}
	fmt.Printf("archive:        %s\n", path)
	fmt.Printf("unpacked size:  %s in %d files\n", humanize.Bytes(info.UnpackedSize), len(info.Files))
	fmt.Printf("decoder memory: %s\n", humanize.Bytes(info.DecoderMemory))
	if avail, ok := platform.MemAvailable(); ok {
		fmt.Printf("MemAvailable:   %s\n", humanize.Bytes(int64(avail)))
	}

	// 7zz first: a child's peak RSS starts at the parent's peak (Go starts it
	// with vfork, and exec keeps the shared memory's high-water mark), which
	// is still small before the built-in engine has run.
	type engine struct{ name, bin string }
	var engines []engine
	if archive.Kind(filepath.Base(path)) != "zip" {
		if bin := app.FindSevenZip(true); bin != "" {
			engines = append(engines, engine{"7zz", bin})
		}
	}
	engines = append(engines, engine{"builtin", ""})
	for _, eng := range engines {
		dst := filepath.Join(filepath.Dir(path), ".dsfetch-bench-"+eng.name)
		_ = os.RemoveAll(dst)
		ex, err := archive.For(path, archive.Options{Password: password, SevenZipBinary: eng.bin})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		start := time.Now()
		err = ex.Extract(context.Background(), path, dst, nil)
		dur := time.Since(start)
		_ = os.RemoveAll(dst)
		if err != nil {
			fmt.Printf("%-8s failed after %s: %v\n", eng.name, dur.Round(time.Millisecond), err)
			continue
		}
		who := syscall.RUSAGE_SELF
		if eng.bin != "" {
			who = syscall.RUSAGE_CHILDREN
		}
		fmt.Printf("%-8s %8.1f s  %7.1f MB/s  peak RSS %s\n", eng.name, dur.Seconds(),
			float64(info.UnpackedSize)/dur.Seconds()/(1<<20), humanize.Bytes(peakRSS(who)))
	}
	return 0
}

// peakRSS returns the maximum resident set size in bytes (Linux reports
// KiB, macOS bytes).
func peakRSS(who int) int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(who, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
