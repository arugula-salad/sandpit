package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestImageEnv(t *testing.T) {
	dir := t.TempDir()
	old := imageMetaPath
	imageMetaPath = filepath.Join(dir, "image.json")
	t.Cleanup(func() { imageMetaPath = old })

	if env := imageEnv(); env != nil {
		t.Fatalf("no file: %q", env)
	}
	os.WriteFile(imageMetaPath, []byte(`{"env":["PATH=/usr/local/go/bin:/usr/bin","HOME=/root","GOPATH=/go","junk"]}`), 0o644)
	env := imageEnv()
	want := []string{"PATH=/usr/local/go/bin:/usr/bin:/usr/local/bin", "GOPATH=/go"}
	if !slices.Equal(env, want) {
		t.Fatalf("imageEnv = %q, want %q", env, want)
	}
	base := baseEnv("/home/sprite", "sprite")
	// The image's PATH wins, with the user's ~/.local/bin last on it too.
	if "PATH="+envValue(base, "PATH") != want[0]+":/home/sprite/.local/bin" || envValue(base, "HOME") != "/home/sprite" || envValue(base, "GOPATH") != "/go" {
		t.Fatalf("baseEnv = %q", base)
	}

	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "data"), nil, 0o644)
	if p, err := lookPath("tool", []string{"PATH=/nonexistent:" + bin}); err != nil || p != filepath.Join(bin, "tool") {
		t.Errorf("lookPath tool = %q, %v", p, err)
	}
	if _, err := lookPath("data", []string{"PATH=" + bin}); err == nil {
		t.Error("a file that is not executable is not a command")
	}
}

// A sprite with no image environment (the default disk) gets the user's
// ~/.local/bin on PATH, after the system directories so it cannot shadow them.
func TestBaseEnvLocalBin(t *testing.T) {
	old := imageMetaPath
	imageMetaPath = filepath.Join(t.TempDir(), "image.json")
	t.Cleanup(func() { imageMetaPath = old })

	got := envValue(baseEnv("/home/sprite", "sprite"), "PATH")
	want := defaultPath + ":/home/sprite/.local/bin"
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

// XDG_STATE_HOME is the XDG default, set so a path written with it expands.
func TestBaseEnvXDGStateHome(t *testing.T) {
	old := imageMetaPath
	imageMetaPath = filepath.Join(t.TempDir(), "image.json")
	t.Cleanup(func() { imageMetaPath = old })
	if got := envValue(baseEnv("/home/sprite", "sprite"), "XDG_STATE_HOME"); got != "/home/sprite/.local/state" {
		t.Fatalf("XDG_STATE_HOME = %q", got)
	}
}
