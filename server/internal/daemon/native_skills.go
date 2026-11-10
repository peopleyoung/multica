package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func (h *nativeToolHost) loadSkill(ctx context.Context, call llm.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if len(call.Arguments) > 64<<10 || decoder.Decode(&args) != nil || args.Name == "" {
		return "", errors.New("load_skill requires a listed skill name and optional relative supporting path")
	}
	var skill *execenv.SkillContextForEnv
	for i := range h.skills {
		if h.skills[i].Name == args.Name {
			skill = &h.skills[i]
			break
		}
	}
	if skill == nil {
		return "", errors.New("skill is not enabled for this run")
	}
	if args.Path == "" || args.Path == "SKILL.md" {
		args.Path = "SKILL.md"
	} else if path.Clean(args.Path) != args.Path || strings.Contains(args.Path, "\\") || strings.HasPrefix(args.Path, "/") || args.Path == ".." || strings.HasPrefix(args.Path, "../") {
		return "", errors.New("skill path must match a bundled relative file")
	}
	content, found := skill.Content, args.Path == "SKILL.md"
	files := make([]string, 0, len(skill.Files))
	for _, file := range skill.Files {
		files = append(files, file.Path)
		if args.Path != "SKILL.md" && file.Path == args.Path {
			content, found = file.Content, true
		}
	}
	if !found {
		return "", errors.New("supporting file is not in the enabled skill bundle")
	}
	if len(content) > 96<<10 {
		return "", errors.New("skill file exceeds native tool output budget")
	}
	raw, err := json.Marshal(map[string]any{"name": skill.Name, "path": args.Path, "content": content, "files": files, "source": "run-authorized skill bundle", "execution": "reading does not execute scripts or grant permissions"})
	if len(raw) > 128<<10 {
		return "", errors.New("skill response exceeds native output budget")
	}
	return string(raw), err
}
