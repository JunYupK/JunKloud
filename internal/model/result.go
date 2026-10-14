package model

import "time"

// Check statuses. PASS and FAIL are the only statuses a problem definition
// may expect; ERROR means the result could not be determined.
const (
	StatusPass  = "PASS"
	StatusFail  = "FAIL"
	StatusError = "ERROR"
)

// Evidence is what a check observed. It never carries Secret plaintext.
type Evidence struct {
	Expected   any           `json:"expected,omitempty"`
	Actual     any           `json:"actual,omitempty"`
	ObservedAt time.Time     `json:"observed_at"`
	UID        string        `json:"uid,omitempty"`
	Generation int64         `json:"generation,omitempty"`
	InputHash  string        `json:"input_hash,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
}

// CheckResult is the outcome of one criterion.
type CheckResult struct {
	CriterionID string   `json:"criterion_id"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Points      int      `json:"points"`
	Evidence    Evidence `json:"evidence"`
}

// Evaluation is the outcome of all criteria of one problem. Score is nil
// whenever any result is ERROR.
type Evaluation struct {
	Results  []CheckResult `json:"results"`
	Score    *int          `json:"score"`
	MaxScore int           `json:"max_score"`
	Complete bool          `json:"complete"`
}

// GradingRun is one execution of the grader for a submission.
type GradingRun struct {
	ID           string     `json:"id"`
	SubmissionID string     `json:"submission_id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   time.Time  `json:"finished_at"`
	Evaluation   Evaluation `json:"evaluation"`
}

// ValidationRecord certifies that a problem package behaved as declared on a
// given grader and environment.
type ValidationRecord struct {
	ProblemHash     string                `json:"problem_hash"`
	GraderVersion   string                `json:"grader_version"`
	EnvironmentHash string                `json:"environment_hash"`
	Seeds           []uint64              `json:"seeds"`
	Passed          bool                  `json:"passed"`
	Cases           map[string]Evaluation `json:"cases"`
}
