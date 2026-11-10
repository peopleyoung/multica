package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const jobLogLimit = 8 << 20

func (j *shellJob) recoverLog() error {
	f, err := os.Open(j.path + ".log")
	if errors.Is(err, os.ErrNotExist) && j.record.LogBytes == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > j.logLimit {
		return errors.New("invalid retained job log")
	}
	if info.Size() < j.record.LogBytes {
		j.record.LogError = "Retained log is shorter than its journal; some output is unavailable."
		j.record.LogTruncated = true
	}
	j.record.LogBytes = info.Size()
	if j.record.OutputStart+int64(len(j.record.Output)) <= info.Size() {
		start := max(int64(0), info.Size()-(16<<10))
		data := make([]byte, info.Size()-start)
		n, err := f.ReadAt(data, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		j.record.OutputStart, j.record.Output = start, data[:n]
	}
	return nil
}

func (j *shellJob) openLog() error {
	if j.path == "" {
		return nil
	}
	f, err := os.OpenFile(j.path+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		j.log = f
	}
	return err
}

func (j *shellJob) appendLog(data []byte) {
	if j.log == nil || j.record.LogError != "" {
		return
	}
	remaining := j.logLimit - j.record.LogBytes
	if int64(len(data)) > remaining {
		data = data[:remaining]
		j.record.LogTruncated = true
	}
	if len(data) == 0 {
		return
	}
	n, err := j.log.Write(data)
	j.record.LogBytes += int64(n)
	if err != nil {
		j.record.LogError = err.Error()
		j.record.LogTruncated = true
	}
}

func (j *shellJob) closeLog() error {
	if j.log == nil {
		return nil
	}
	err := errors.Join(j.log.Sync(), j.log.Close())
	j.log = nil
	return err
}

func (j *shellJob) readLog(offset int64, limit int) ([]byte, error) {
	f, err := os.Open(j.path + ".log")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, min(int64(limit), j.record.LogBytes-offset))
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:n], nil
}

func (j *shellJob) search(offset int64, text string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.path == "" {
		return "", errors.New("job log storage is not configured")
	}
	if offset > j.record.LogBytes {
		return "", errors.New("offset exceeds retained log")
	}
	// Scan at most one MiB, retaining overlap so a match cannot disappear
	// between pages. Offsets refer to original bytes, never rendered UTF-8.
	data, err := j.readLog(offset, 1<<20)
	if err != nil {
		return "", err
	}
	matches := []map[string]any{}
	next := offset + int64(len(data))
	for cursor := 0; cursor < len(data); {
		index := bytes.Index(data[cursor:], []byte(text))
		if index < 0 {
			break
		}
		index += cursor
		matches = append(matches, map[string]any{"offset": offset + int64(index), "preview": string(bytes.ToValidUTF8(data[max(0, index-100):min(len(data), index+len(text)+100)], []byte("�")))})
		cursor = index + len(text)
		if len(matches) == 20 {
			next = offset + int64(cursor)
			break
		}
	}
	if len(matches) < 20 && next < j.record.LogBytes {
		next = max(offset+1, next-int64(len(text)-1))
	}
	raw, err := json.Marshal(map[string]any{"job_id": jobHandle(j.record.ID), "matches": matches, "next_offset": next, "log_bytes": j.record.LogBytes, "log_truncated": j.record.LogTruncated, "log_error": j.record.LogError})
	return string(raw), err
}
