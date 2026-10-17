//go:build linux

package session

import (
	"maps"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// SessionView is what a caller (the CLI) is handed of a session by Prepare,
// PrepareWithReport and Status: the learner-safe part of the record. It never
// carries the grading definition (criteria and their expected values, the
// declared starting state, setup, solution and case actions, explanation or
// reference answer: spec 6.3), and the English questions and provided files
// only once the session is started (spec 9.1). The full model.SessionRecord
// stays inside this package and the store; grading and review load it
// themselves.
type SessionView struct {
	ID            string
	EnvironmentID string
	Mode          string
	State         string
	ParentID      string
	Seed          uint64
	// StartedAt is nil until the clock starts.
	StartedAt     *time.Time
	DurationLimit time.Duration
	// The versions the session was prepared with (environment information
	// the readiness screen may show).
	GraderVersion       string
	EnvironmentHash     string
	EnvironmentManifest map[string]string
	Problems            []ProblemView
}

// ProblemView is the learner-safe part of one frozen problem.
type ProblemView struct {
	ID        string
	Title     string
	Domain    string
	Points    int // the sum of the problem's criterion points (shown on the exam)
	Namespace string
	Context   string
	Marked    bool
	// Question and Assets (the English question and the provided files,
	// rendered) are empty until the session is started.
	Question string
	Assets   map[string][]byte
}

// viewOf builds the caller's view of a record.
func viewOf(rec model.SessionRecord) SessionView {
	v := SessionView{
		ID: rec.ID, EnvironmentID: rec.EnvironmentID, Mode: rec.Mode, State: rec.State, ParentID: rec.ParentID,
		Seed: rec.Seed, DurationLimit: rec.DurationLimit,
		GraderVersion: rec.GraderVersion, EnvironmentHash: rec.EnvironmentHash, EnvironmentManifest: maps.Clone(rec.EnvironmentManifest),
	}
	if rec.StartedAt != nil {
		at := *rec.StartedAt
		v.StartedAt = &at
	}
	for _, f := range rec.Problems {
		p := ProblemView{
			ID: f.Definition.ID, Title: f.Definition.Title, Domain: f.Definition.Domain, Points: maxPoints(f),
			Namespace: f.Namespace, Context: f.Context, Marked: rec.Marks[f.Definition.ID],
		}
		if rec.StartedAt != nil {
			p.Question, p.Assets = f.Question, maps.Clone(f.Assets)
		}
		v.Problems = append(v.Problems, p)
	}
	return v
}
