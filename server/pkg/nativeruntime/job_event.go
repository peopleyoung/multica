package nativeruntime

import (
	"errors"
	"regexp"
)

type JobEvent struct {
	Key      string `json:"key"`
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

var eventKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var eventJobPattern = regexp.MustCompile(`^[a-z0-9_-]{1,100}$`)

func (e JobEvent) Validate() error {
	if !eventKeyPattern.MatchString(e.Key) || !eventJobPattern.MatchString(e.JobID) {
		return errors.New("invalid job event identity")
	}
	switch e.Status {
	case "completed", "failed", "cancelled", "interrupted":
		return nil
	}
	return errors.New("job event must report a terminal observation")
}
