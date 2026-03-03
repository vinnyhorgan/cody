package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDir(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/a/b/c"
	ensureDir(path)
	// Should not panic
}

func TestSafeFilenameSpecialChars(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal", "normal"},
		{"has spaces", "has_spaces"},
		{"path/to/file", "path_to_file"},
		{"colon:here", "colon_here"},
		{"back\\slash", "back_slash"},
	}
	for _, tt := range tests {
		got := safeFilename(tt.input)
		if got != tt.want {
			t.Errorf("safeFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTimestampFormat(t *testing.T) {
	ts := timestamp()
	if len(ts) < 20 {
		t.Errorf("timestamp too short: %q", ts)
	}
}

func TestSyncTemplates(t *testing.T) {
	dir := t.TempDir()
	if err := syncTemplates(dir); err != nil {
		t.Fatalf("syncTemplates: %v", err)
	}

	// Verify some expected files exist from templates
	if _, err := os.Stat(filepath.Join(dir, "SOUL.md")); err != nil {
		t.Error("SOUL.md should be synced")
	}
	// Verify skills dir is synced
	if _, err := os.Stat(filepath.Join(dir, "skills")); err != nil {
		t.Error("skills dir should be synced")
	}
}

func TestSyncTemplatesIdempotent(t *testing.T) {
	dir := t.TempDir()
	syncTemplates(dir)

	// Write a custom file that should NOT be overwritten
	customPath := filepath.Join(dir, "SYSTEM.md")
	os.WriteFile(customPath, []byte("custom content"), 0644)

	syncTemplates(dir) // second sync

	data, _ := os.ReadFile(customPath)
	if string(data) != "custom content" {
		t.Error("syncTemplates should not overwrite existing files")
	}
}

func TestCopyEmbeddedDir(t *testing.T) {
	dir := t.TempDir()
	err := copyEmbeddedDir(templatesFS, "templates", dir)
	if err != nil {
		t.Fatalf("copyEmbeddedDir: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Error("expected files to be copied")
	}
}
