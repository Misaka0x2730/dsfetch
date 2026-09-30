package ui

import (
	"runtime"
	"strings"
	"testing"

	"dsfetch/internal/version"
)

func TestAboutShowsBuildDate(t *testing.T) {
	defer func(d string) { version.BuildDate = d }(version.BuildDate)
	e := &Env{DataDir: "/data"}
	lastLine := func() string {
		lines := strings.Split(e.aboutText(), "\n")
		return lines[len(lines)-1]
	}
	plain := runtime.GOOS + "/" + runtime.GOARCH + ", " + version.GitCommit

	version.BuildDate = "2026-09-28T11:17:44Z" // as the Docker build sets it
	if got, want := lastLine(), plain+", 2026-09-28 11:17 UTC"; got != want {
		t.Errorf("build line = %q, want %q", got, want)
	}
	version.BuildDate = "unknown" // dev build: no date
	if got := lastLine(); got != plain {
		t.Errorf("dev build line = %q, want %q", got, plain)
	}
}
