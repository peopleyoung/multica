// Package tools owns bounded native tools. Filesystem confinement is not an OS
// sandbox. Commands are unconfined unless their host configures a sandbox.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/llm"
)

const outputLimit = 64 << 10
const fileLimit = 1 << 20

// ErrOutcomeUnknown means an operation may have committed despite a failed
// response. The engine must preserve uncertainty and require verification
// instead of reporting failure or automatically replaying the operation.
var ErrOutcomeUnknown = errors.New("tool outcome is unknown; reconcile the operation before continuing")

type Permissions struct {
	WriteFiles bool
	// Shell also grants network and filesystem access outside the workdir.
	// No command blacklist is presented as protection against these powers.
	Shell bool
}

type Host struct {
	root        *os.Root
	permissions Permissions
	jobs        *shellJobs
	terminals   *terminalSet
}

func New(workDir string, permissions Permissions, env []string, jobDir string) (*Host, error) {
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, err
	}
	jobs, err := openShellJobs(workDir, env, jobDir)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	terminals := &terminalSet{sessions: map[string]*terminalSession{}, stop: make(chan struct{})}
	go terminals.reapLoop()
	return &Host{root: root, permissions: permissions, jobs: jobs, terminals: terminals}, nil
}

func (h *Host) Close() error {
	terminalErr := h.terminals.close()
	h.jobs.close()
	return errors.Join(terminalErr, h.root.Close())
}

// Unknown tools are never assumed safe to treat as repeatable reads.
func (h *Host) IsReadOnly(call llm.ToolCall) bool {
	switch call.Name {
	case "read_rules", "read_file", "list_directory", "search_text":
		return true
	}
	return false
}

// ReadArtifact shares the confined regular-file read and one MiB limit.
func (h *Host) ReadArtifact(path string) ([]byte, error) { return h.read(path) }

func (h *Host) Definitions() []llm.Tool { return environmentDefinitions(h.permissions) }

func environmentDefinitions(permissions Permissions) []llm.Tool {
	defs := []llm.Tool{
		{Name: "read_rules", Description: "Read AGENTS.md rules from the work directory through a relative target directory, in precedence order. Read before modifying files beneath that directory.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		{Name: "read_file", Description: "Read a relative regular text file. Small files return text; larger files or explicit offset/limit return JSON with output and next_offset. Continue with that absolute byte offset. Each page is at most 65536 bytes.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":65536}},"required":["path"],"additionalProperties":false}`)},
		{Name: "list_directory", Description: "List entries relative to the work directory.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		{Name: "search_text", Description: "Find literal text in regular files beneath the work directory; skips symlinks and large files.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path","text"],"additionalProperties":false}`)},
	}
	if permissions.WriteFiles {
		defs = append(defs, llm.Tool{Name: "make_directory", Description: "Create a relative directory and missing parents within the work directory; existing directories are accepted.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)})
		defs = append(defs, llm.Tool{Name: "apply_patch", Description: "Replace one exact occurrence of old_text in a regular UTF-8 file. Empty old_text creates a new file only. Parent directory must exist.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}},"required":["path","old_text","new_text"],"additionalProperties":false}`)})
	}
	if permissions.Shell {
		defs = append(defs, shellJobDefinitions()...)
		defs = append(defs, terminalDefinition())
	}
	return defs
}

// Execute reports tool failures as errors for the engine to pair with the call
// ID and return to the model. The engine owns checkpoints, not the host.
func (h *Host) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(call.Arguments) > fileLimit {
		return "", errors.New("tool arguments exceed limit")
	}
	if call.Name == "terminal" {
		return h.terminal(ctx, call)
	}
	if call.Name == "read_file" {
		return h.readPage(ctx, call.Arguments)
	}
	if isShellJobTool(call.Name) {
		if !h.permissions.Shell {
			return "", errors.New("shell execution is not authorized")
		}
		return h.jobs.execute(ctx, call)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args == nil {
		return "", errors.New("tool arguments must be a JSON object")
	}
	allowed := map[string]bool{}
	var required []string
	switch call.Name {
	case "read_file", "list_directory", "read_rules":
		required = []string{"path"}
	case "search_text":
		required = []string{"path", "text"}
	case "make_directory", "apply_patch":
		if !h.permissions.WriteFiles {
			return "", errors.New("file writes are not authorized")
		}
		required = []string{"path"}
		if call.Name == "apply_patch" {
			required = []string{"path", "old_text", "new_text"}
		}
	default:
		return "", errors.New("unknown tool")
	}
	values := make(map[string]string)
	for _, key := range required {
		allowed[key] = true
		raw, ok := args[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return "", fmt.Errorf("tool argument %s is required", key)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("tool argument %s must be a string", key)
		}
		values[key] = value
	}
	for key := range args {
		if !allowed[key] {
			return "", errors.New("unknown tool argument")
		}
	}
	path := values["path"]
	if path == "" || !filepath.IsLocal(path) {
		return "", errors.New("tool path must be relative to the work directory")
	}
	switch call.Name {
	case "make_directory":
		if err := h.root.MkdirAll(path, 0700); err != nil {
			return "", err
		}
		return "Directory created.", nil
	case "read_rules":
		return h.Rules(ctx, path)
	case "read_file":
		data, err := h.read(path)
		if err != nil {
			return "", err
		}
		return bounded(string(data)), nil
	case "list_directory":
		file, err := h.root.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		entries, err := file.ReadDir(1001)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		var out strings.Builder
		for i, entry := range entries {
			if i == 1000 {
				out.WriteString("[entry limit reached]\n")
				break
			}
			fmt.Fprintln(&out, entry.Name())
			if out.Len() > outputLimit {
				break
			}
		}
		return bounded(out.String()), nil
	case "search_text":
		return h.search(ctx, path, values["text"])
	case "apply_patch":
		return h.patch(ctx, path, values["old_text"], values["new_text"])
	}
	return "", errors.New("unknown tool")
}

func (h *Host) read(path string) ([]byte, error) {
	f, err := openForRead(h.root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("tool requires a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, fileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > fileLimit {
		return nil, errors.New("file exceeds read limit")
	}
	return data, nil
}

func (h *Host) search(ctx context.Context, path, text string) (string, error) {
	if text == "" {
		return "", errors.New("search text must not be empty")
	}
	var out strings.Builder
	visited := 0
	stop := errors.New("search limit")
	err := fs.WalkDir(h.root.FS(), filepath.ToSlash(filepath.Clean(path)), func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 10000 || out.Len() > outputLimit {
			return stop
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := h.read(filepath.FromSlash(name))
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, text) {
				fmt.Fprintf(&out, "%s:%d:%s\n", name, i+1, line)
				if out.Len() > outputLimit {
					return stop
				}
			}
		}
		return nil
	})
	if errors.Is(err, stop) {
		out.WriteString("\n[search limit reached]")
		err = nil
	}
	return bounded(out.String()), err
}

func (h *Host) patch(ctx context.Context, path, oldText, newText string) (string, error) {
	if len(newText) > fileLimit {
		return "", errors.New("patch exceeds file limit")
	}
	var data []byte
	mode := os.FileMode(0600)
	info, err := h.root.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("patch requires a regular file, not a symlink")
		}
		if oldText == "" {
			return "", errors.New("empty old_text only creates a new file")
		}
		mode = info.Mode().Perm()
		data, err = h.read(path)
		if err != nil {
			return "", err
		}
		if strings.Count(string(data), oldText) != 1 {
			return "", errors.New("old_text must match exactly one occurrence")
		}
		data = []byte(strings.Replace(string(data), oldText, newText, 1))
	} else if errors.Is(err, os.ErrNotExist) {
		if oldText != "" {
			return "", errors.New("cannot replace text in a missing file")
		}
		data = []byte(newText)
	} else {
		return "", err
	}
	if len(data) > fileLimit {
		return "", errors.New("patched file exceeds limit")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if oldText == "" {
		file, err := h.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return "", err
		}
	} else {
		temp := filepath.Join(filepath.Dir(path), ".multica-patch-"+uuid.NewString())
		file, err := h.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		defer h.root.Remove(temp)
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return "", err
		}
		if err := h.root.Rename(temp, path); err != nil {
			return "", err
		}
	}
	return "Patch applied.", nil
}

func bounded(s string) string {
	if len(s) > outputLimit {
		return strings.ToValidUTF8(s[:outputLimit], "�") + "\n[output truncated]"
	}
	return strings.ToValidUTF8(s, "�")
}
