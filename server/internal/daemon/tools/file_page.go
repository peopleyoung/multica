package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func (h *Host) readPage(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset *int64 `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", errors.New("tool arguments must be a JSON object")
	}
	for key, value := range fields {
		if (key != "path" && key != "offset" && key != "limit") || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", errors.New("unknown or null file argument")
		}
	}
	if json.Unmarshal(raw, &args) != nil || args.Path == "" || !filepath.IsLocal(args.Path) {
		return "", errors.New("file path must be relative to the work directory")
	}
	offset, limit := int64(0), outputLimit
	if args.Offset != nil {
		offset = *args.Offset
	}
	if args.Limit != nil {
		limit = *args.Limit
	}
	if offset < 0 || limit < 1 || limit > outputLimit {
		return "", errors.New("offset must be nonnegative; limit must be between 1 and 65536 bytes")
	}
	f, err := openForRead(h.root, args.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("tool requires a regular file")
	}
	if offset > info.Size() {
		return "", errors.New("offset exceeds file size")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data := make([]byte, min(int64(limit), info.Size()-offset))
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	data = data[:n]
	if n == 0 && offset < info.Size() {
		return "", errors.New("file changed while reading; read its current state again")
	}
	if args.Offset == nil && args.Limit == nil && info.Size() <= outputLimit {
		return strings.ToValidUTF8(string(data), "�"), nil
	}
	for {
		// Continuation offsets never split a valid UTF-8 code point. Explicit
		// offsets into the middle of one must be corrected by the caller.
		for removed := 0; len(data) > 0 && removed < 3; removed++ {
			r, size := utf8.DecodeLastRune(data)
			if r != utf8.RuneError || size != 1 || data[len(data)-1] < 128 {
				break
			}
			data = data[:len(data)-1]
		}
		if len(data) == 0 && offset < info.Size() {
			return "", errors.New("page limit or offset splits a UTF-8 character; increase limit or use the previous next_offset")
		}
		if !utf8.Valid(data) {
			return "", errors.New("file page is not UTF-8 text; use a binary-aware tool or a valid byte offset")
		}
		result, err := json.Marshal(map[string]any{"path": args.Path, "output": strings.ToValidUTF8(string(data), "�"), "output_start": offset, "next_offset": offset + int64(len(data)), "file_bytes": info.Size(), "has_more": offset+int64(len(data)) < info.Size(), "continuation": "Use read_file with path and next_offset as offset. Offsets count original bytes."})
		if err != nil {
			return "", err
		}
		if len(result) <= outputLimit+512 {
			return string(result), nil
		}
		if len(data) <= 1 {
			return "", errors.New("file path metadata exceeds output limit")
		}
		data = data[:max(1, len(data)*outputLimit/len(result)-256)]
	}
}
