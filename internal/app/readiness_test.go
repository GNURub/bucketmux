package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritableDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := writableDirectory(dir); err != nil {
		t.Fatalf("writableDirectory() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("readiness probe left temporary files: %v", entries)
	}
}

func TestWritableDirectoryRejectsNonDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := writableDirectory(path); err == nil {
		t.Fatal("writableDirectory() unexpectedly accepted a file")
	}
}
