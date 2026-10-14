package model

import "time"

// Session modes.
const (
	ModePractice = "practice"
	ModeRetry    = "retry"
	ModeMock     = "mock"
)

// SessionRecord is one practice, retry or mock-exam session. StartedAt is nil
// until the clock starts. Marks holds the learner's per-problem flags.
type SessionRecord struct {
	ID            string          `json:"id"`
	EnvironmentID string          `json:"environment_id"`
	Mode          string          `json:"mode"`
	State         string          `json:"state"`
	ParentID      string          `json:"parent_id,omitempty"`
	Seed          uint64          `json:"seed"`
	Problems      []FrozenProblem `json:"problems"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	DurationLimit time.Duration   `json:"duration_limit,omitempty"`
	Marks         map[string]bool `json:"marks,omitempty"`
}

// Submission is one grading request and the hashes of the inputs it saw.
type Submission struct {
	ID                 string            `json:"id"`
	SessionID          string            `json:"session_id"`
	Kind               string            `json:"kind"`
	ParentSubmissionID string            `json:"parent_submission_id,omitempty"`
	SubmittedAt        time.Time         `json:"submitted_at"`
	InputHashes        map[string]string `json:"input_hashes,omitempty"`
}
