// Package jobs tracks background jobs.
package jobs

// Status of a background job.
type Status string

// Job statuses.
const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
)

// Job is a background job.
type Job struct {
	Name   string
	Status Status
}

// Finished reports whether the job status is final.
func (j Job) Finished() bool { return j.Status == StatusDone }
