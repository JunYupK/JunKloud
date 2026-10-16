package model

import "time"

// Session modes.
const (
	ModePractice = "practice"
	ModeRetry    = "retry"
	ModeMock     = "mock"
)

// Session states (spec section 9.1). The normal flow is PREPARING, RUNNING,
// GRADING, COMPLETED. SETUP_ERROR and GRADING_ERROR record failures and
// ABANDONED records a session the learner ended to start over.
const (
	StatePreparing    = "PREPARING"
	StateRunning      = "RUNNING"
	StateGrading      = "GRADING"
	StateCompleted    = "COMPLETED"
	StateSetupError   = "SETUP_ERROR"
	StateGradingError = "GRADING_ERROR"
	StateAbandoned    = "ABANDONED"
)

// SessionStates lists every valid session state.
func SessionStates() []string {
	return []string{
		StatePreparing, StateRunning, StateGrading, StateCompleted,
		StateSetupError, StateGradingError, StateAbandoned,
	}
}

// ActiveSessionStates lists the states in which a session still occupies its
// environment: a session being prepared, running or graded, and the two
// failure states whose resources and recovery the learner has not resolved
// yet (SETUP_ERROR, GRADING_ERROR). COMPLETED (resubmits create new
// submissions, never new cluster work) and ABANDONED are not active. Bank
// validation and other environment-wide mutations refuse to run while any
// session of the environment is active.
func ActiveSessionStates() []string {
	return []string{StatePreparing, StateRunning, StateGrading, StateGradingError, StateSetupError}
}

// SessionRecord is one practice, retry or mock-exam session. StartedAt is nil
// until the clock starts. Marks holds the learner's per-problem flags.
// GraderVersion, EnvironmentHash and EnvironmentManifest are frozen when the
// session is prepared: the grader version and the environment version
// manifest (with its hash) the problems were certified against. They are
// empty for sessions that predate them.
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

	GraderVersion       string            `json:"grader_version,omitempty"`
	EnvironmentHash     string            `json:"environment_hash,omitempty"`
	EnvironmentManifest map[string]string `json:"environment_manifest,omitempty"`
}

// Submission kinds. A whole-session submission covers every problem of the
// session (a mock exam's only kind); a problem submission covers one problem
// of a practice session and does not end the session.
const (
	SubmissionSession = "session"
	SubmissionProblem = "problem"
)

// Submission is one grading request and the hashes of the inputs it saw.
//
// SolveDuration is the learner's time from the session start to SubmittedAt;
// the time the grader then takes is a property of the grading runs and is
// never part of it. Overtime is the part of SolveDuration beyond the session's
// limit, WithinLimit says whether the submission met the limit (always true
// for a session without one), and ClockAnomaly is true when SubmittedAt lies
// before the session start, i.e. the wall clock ran backwards (SolveDuration is
// then zero). ProblemID is empty for a whole-session submission.
// ViewedSolutions lists the problems whose solution the learner had already
// opened when this submission was made, so a repeated attempt is never shown
// as a first look (spec 9.2).
type Submission struct {
	ID                 string            `json:"id"`
	SessionID          string            `json:"session_id"`
	Kind               string            `json:"kind"`
	ProblemID          string            `json:"problem_id,omitempty"`
	ParentSubmissionID string            `json:"parent_submission_id,omitempty"`
	SubmittedAt        time.Time         `json:"submitted_at"`
	SolveDuration      time.Duration     `json:"solve_duration,omitempty"`
	Overtime           time.Duration     `json:"overtime,omitempty"`
	WithinLimit        bool              `json:"within_limit"`
	ClockAnomaly       bool              `json:"clock_anomaly,omitempty"`
	ViewedSolutions    []string          `json:"viewed_solutions,omitempty"`
	InputHashes        map[string]string `json:"input_hashes,omitempty"`
}

// ReviewEvent records that the learner opened the explanation and solution of
// one problem.
type ReviewEvent struct {
	SessionID  string    `json:"session_id"`
	ProblemID  string    `json:"problem_id"`
	ReviewedAt time.Time `json:"reviewed_at"`
}
