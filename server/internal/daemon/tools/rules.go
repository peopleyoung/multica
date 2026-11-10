package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Rules reads only the workdir-to-path AGENTS.md hierarchy. It never walks
// into the daemon home, follows outside symlinks, or scans unrelated trees.
func (h *Host) Rules(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "."
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) || strings.ContainsRune(path, 0) {
		return "", errors.New("rules path must stay within the work directory")
	}
	type rule struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	rules := make([]rule, 0)
	dirs := []string{"."}
	if path != "." {
		for _, part := range strings.Split(filepath.ToSlash(path), "/") {
			dirs = append(dirs, filepath.Join(dirs[len(dirs)-1], part))
		}
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file := filepath.Join(dir, "AGENTS.md")
		content, err := h.read(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !utf8.Valid(content) {
			return "", errors.New("repository rules require UTF-8")
		}
		rules = append(rules, rule{Path: filepath.ToSlash(file), Content: string(content)})
	}
	raw, err := json.Marshal(rules)
	if len(raw) > outputLimit {
		return "", errors.New("repository rules exceed read budget; narrow their content before running")
	}
	return string(raw), err
}
