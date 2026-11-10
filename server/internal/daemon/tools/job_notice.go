package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/pkg/llm"
)

// JobNotifications derives delivery from committed model history. A failed
// checkpoint cannot consume a notification or an output cursor.
func (h *Host) JobNotifications(ctx context.Context, history []llm.ModelMessage) ([]string, error) {
	if !h.permissions.Shell {
		return nil, nil
	}
	owner := jobOwner(ctx)
	h.jobs.mu.Lock()
	if err := h.jobs.load(owner); err != nil {
		h.jobs.mu.Unlock()
		return nil, err
	}
	jobs := []*shellJob{}
	for _, job := range h.jobs.jobs {
		if job.record.Owner == owner {
			jobs = append(jobs, job)
		}
	}
	h.jobs.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].record.ID < jobs[j].record.ID })
	notices := []string{}
	for _, job := range jobs {
		job.mu.Lock()
		id, status := jobHandle(job.record.ID), job.record.Status
		end := job.record.OutputStart + int64(len(job.record.Output))
		job.mu.Unlock()
		if liveJob(status) {
			continue
		}
		marker := "[native job completed " + id + "]"
		seen := false
		for _, message := range history {
			if message.Role == "user" && strings.HasPrefix(message.Content, marker) {
				seen = true
				break
			}
			if message.Role == "tool" {
				var result struct {
					ID     string `json:"job_id"`
					Status string `json:"status"`
				}
				if json.Unmarshal([]byte(message.Content), &result) == nil && result.ID == id && result.Status != "" && !liveJob(result.Status) {
					seen = true
					break
				}
			}
		}
		if seen {
			continue
		}
		output, err := job.read(max(int64(0), end-1024))
		if err != nil {
			return nil, err
		}
		notices = append(notices, marker+"\n"+output+"\nInspect job_output or job_search before interpreting completion. Never relaunch to query progress.")
		if len(notices) == 8 {
			break
		}
	}
	return notices, nil
}
