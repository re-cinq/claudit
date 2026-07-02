package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadWrite(t *testing.T) {
	// Create a temp git repo so GetProjectRoot works
	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	shiftlogDir := filepath.Join(tmpDir, ".shiftlog")
	if err := os.Mkdir(shiftlogDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Change to temp dir so git rev-parse works
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	cfg := &Config{NotesRef: "refs/notes/test", Debug: true}
	if err := Write(cfg); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	got, err := Read()
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if got.NotesRef != "refs/notes/test" {
		t.Errorf("NotesRef = %q, want %q", got.NotesRef, "refs/notes/test")
	}
	if !got.Debug {
		t.Error("Debug = false, want true")
	}
}

func TestReadMissing(t *testing.T) {
	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	cfg, err := Read()
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if cfg.Debug {
		t.Error("Debug = true for missing config, want false")
	}
}

func TestDirExists(t *testing.T) {
	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	exists, err := DirExists()
	if err != nil {
		t.Fatalf("DirExists failed: %v", err)
	}
	if exists {
		t.Error("DirExists = true before creating .shiftlog")
	}

	os.Mkdir(filepath.Join(tmpDir, ".shiftlog"), 0755)

	exists, err = DirExists()
	if err != nil {
		t.Fatalf("DirExists failed: %v", err)
	}
	if !exists {
		t.Error("DirExists = false after creating .shiftlog")
	}
}
