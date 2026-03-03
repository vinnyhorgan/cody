package main

import (
	"embed"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

//go:embed templates
var templatesFS embed.FS

//go:embed skills
var skillsFS embed.FS

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func safeFilename(name string) string {
	return unsafeChars.ReplaceAllString(name, "_")
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// syncTemplates copies embedded template and skill files to the workspace if they don't exist.
func syncTemplates(workspace string) error {
	dirs := []struct {
		fs     embed.FS
		prefix string
		dest   string
	}{
		{templatesFS, "templates", workspace},
		{skillsFS, "skills", filepath.Join(workspace, "skills")},
	}
	for _, d := range dirs {
		if err := copyEmbeddedDir(d.fs, d.prefix, d.dest); err != nil {
			return err
		}
	}
	return nil
}

func copyEmbeddedDir(fsys embed.FS, root, dest string) error {
	entries, err := fsys.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := root + "/" + e.Name()
		dst := filepath.Join(dest, e.Name())
		if e.IsDir() {
			if err := ensureDir(dst); err != nil {
				return err
			}
			if err := copyEmbeddedDir(fsys, src, dst); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue // don't overwrite existing files
		}
		data, err := fsys.ReadFile(src)
		if err != nil {
			return err
		}
		if err := ensureDir(filepath.Dir(dst)); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return err
		}
	}
	return nil
}
