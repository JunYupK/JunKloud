//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// failureBudget bounds the bookkeeping that runs after a failed or cancelled
// preparation, so the session still ends up SETUP_ERROR.
const failureBudget = time.Minute

// Prepare readies a session without revealing it (spec 9.1 PREPARING).
//
// Under the environment lock it:
//
//  1. refuses a second unfinished session and verifies the environment
//     (Ensure); the environment version manifest, its hash and the grader
//     version are computed now and frozen on the session;
//  2. decides which problems are servable (serve.go): a current validation
//     certificate for this grader and environment, PASSing doctor
//     capabilities for the capabilities they declare, no provided file
//     named like the question. A mock session (exam file order and duration)
//     is refused with every reason if any of its problems is not servable;
//     a practice session draws with problem.Select from the servable
//     problems only and PrepareWithReport says which were left out;
//  3. freezes every problem with the session seed, stores the private package
//     under its content hash and records the session as PREPARING;
//  4. applies every problem's setup (scope enforced, the inventory recorded by
//     the Manager) and evaluates each initial state with the runtime grader,
//     comparing it with the problem's declared starting state. An
//     intentionally broken app matches its declaration and is the expected
//     result. An ERROR result, a different state or any failed setup makes
//     the session SETUP_ERROR; nothing is deleted, the inventory stays for
//     recovery.
//
// Nothing public is written: no question, no provided file, and the clock does
// not start (StartedAt is nil) until Start. The returned record carries no
// question text or provided files. On a failure after the session was
// recorded, the returned record (state SETUP_ERROR) is returned with the
// error, which wraps ErrSetupFailed.
func (s *Service) Prepare(ctx context.Context, opts StartOptions) (model.SessionRecord, error) {
	rec, _, err := s.PrepareWithReport(ctx, opts)
	return rec, err
}

// PrepareWithReport is Prepare that also returns what was left out of the
// draw (Report.Excluded). The report is valid with or without an error.
func (s *Service) PrepareWithReport(ctx context.Context, opts StartOptions) (model.SessionRecord, Report, error) {
	rec, report, err := s.prepare(ctx, opts)
	return rec, report, err
}

func (s *Service) prepare(ctx context.Context, opts StartOptions) (model.SessionRecord, Report, error) {
	seed := opts.Seed
	if seed == 0 {
		var err error
		if seed, err = s.cfg.NewSeed(); err != nil {
			return model.SessionRecord{}, Report{}, err
		}
	}
	pl, err := s.plan(opts)
	if err != nil {
		return model.SessionRecord{}, Report{}, err
	}

	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if errors.Is(err, lock.ErrLocked) {
		return model.SessionRecord{}, Report{}, fmt.Errorf("session: prepare: another start, reset, validation or grade is running on this environment: %w", err)
	}
	if err != nil {
		return model.SessionRecord{}, Report{}, fmt.Errorf("session: prepare: %w", err)
	}
	defer release()

	if err := s.requireNoActiveSession(ctx); err != nil {
		return model.SessionRecord{}, Report{}, err
	}
	id, err := s.cfg.Env.Ensure(ctx)
	if err != nil {
		return model.SessionRecord{}, Report{}, fmt.Errorf("session: verify the environment: %w", err)
	}
	if id.ID != s.cfg.Identity.ID {
		return model.SessionRecord{}, Report{}, fmt.Errorf("%w: found %q, expected %q; start the program again", ErrEnvironmentChanged, id.ID, s.cfg.Identity.ID)
	}
	manifest, err := s.cfg.EnvironmentManifest(ctx, id)
	if err != nil {
		return model.SessionRecord{}, Report{}, fmt.Errorf("session: compute the environment manifest: %w", err)
	}
	envHash := author.EnvironmentHash(manifest)
	chosen, report, err := s.serve(ctx, id, envHash, pl, opts, seed)
	if err != nil {
		return model.SessionRecord{}, report, err
	}
	limit := pl.limit
	rec, err := s.record(ctx, id, opts, seed, limit, chosen, manifest, envHash)
	return rec, report, err
}

// record freezes the chosen problems, stores the session and carries the
// preparation out. The caller holds the lock and has verified the
// environment and the problems.
func (s *Service) record(ctx context.Context, id model.Identity, opts StartOptions, seed uint64, limit time.Duration, chosen []model.Problem, manifest map[string]string, envHash string) (model.SessionRecord, error) {
	frozen, setups, err := s.freeze(id, chosen, seed)
	if err != nil {
		return model.SessionRecord{}, err
	}
	for _, p := range chosen {
		if _, err := StorePackage(s.cfg.StateDir, p); err != nil {
			return model.SessionRecord{}, err
		}
	}

	sid, err := s.cfg.NewSessionID(opts.Mode)
	if err != nil {
		return model.SessionRecord{}, err
	}
	rec := model.SessionRecord{
		ID: sid, EnvironmentID: id.ID, Mode: opts.Mode, State: model.StatePreparing,
		Seed: seed, Problems: frozen, DurationLimit: limit,
		GraderVersion: s.cfg.GraderVersion, EnvironmentHash: envHash, EnvironmentManifest: manifest,
	}
	if err := s.cfg.Store.CreateSession(ctx, rec); err != nil {
		return model.SessionRecord{}, fmt.Errorf("session: record the session: %w", err)
	}
	op := model.Operation{
		ID: OperationID(sid), EnvironmentID: id.ID, SessionID: sid, Kind: OperationKind,
		Phase: PhasePlanned, Planned: model.Inventory{EnvironmentID: id.ID, SessionID: sid},
		CreatedAt: s.cfg.Now().UTC(),
	}
	if err := s.cfg.Store.PutOperation(ctx, op); err != nil {
		return s.fail(rec, op, fmt.Errorf("record the preparation: %w", err), "")
	}

	// Setup first for every problem, then the initial evaluations: the
	// problems start deploying together instead of one after the other.
	for i, f := range frozen {
		owner := environment.Owner{
			Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: sid, ProblemID: f.Definition.ID},
			Namespace:        f.Namespace,
			Scope:            f.Definition.Scope,
			CreateNamespaces: true,
		}
		if _, err := s.cfg.Env.Apply(ctx, id, owner, setups[i]); err != nil {
			return s.fail(rec, op, fmt.Errorf("problem %q: the setup could not be applied", f.Definition.ID),
				fmt.Sprintf("problem %q: apply the setup: %v", f.Definition.ID, err), err)
		}
	}
	op.Phase = PhaseSetupApplied
	if err := s.cfg.Store.PutOperation(ctx, op); err != nil {
		return s.fail(rec, op, fmt.Errorf("record the preparation: %w", err), "")
	}

	if err := s.verifyInitialStates(ctx, id, rec); err != nil {
		var vf *verifyFailure
		if errors.As(err, &vf) {
			return s.fail(rec, op, vf.public(), vf.detail, err)
		}
		return s.fail(rec, op, errors.New("the initial states could not be evaluated"), err.Error(), err)
	}

	op.Phase = PhaseDone
	if err := s.cfg.Store.PutOperation(ctx, op); err != nil {
		return s.fail(rec, op, fmt.Errorf("record the preparation: %w", err), "")
	}
	ready, err := s.cfg.Store.LoadSession(ctx, sid)
	if err != nil {
		return model.SessionRecord{}, fmt.Errorf("session: reload the prepared session: %w", err)
	}
	return hidePublic(ready), nil
}

// requireNoActiveSession refuses a new session while another one still
// occupies the environment (including a failed preparation whose recovery is
// pending). Callers hold the lock, so no session can appear in between.
func (s *Service) requireNoActiveSession(ctx context.Context) error {
	active, err := s.cfg.Store.ActiveSessions(ctx, s.cfg.Identity.ID)
	if err != nil {
		return fmt.Errorf("session: look for active sessions: %w", err)
	}
	if len(active) == 0 {
		return nil
	}
	var list []string
	for _, a := range active {
		list = append(list, fmt.Sprintf("%s (%s, %s)", a.ID, a.Mode, a.State))
	}
	return fmt.Errorf("%w: finish or abandon %s first", ErrSessionActive, strings.Join(list, ", "))
}

// freeze fixes every problem with the session seed and renders its setup
// actions, before anything is recorded or applied. Two problems may not own
// the same namespace: problems never depend on each other's resources.
func (s *Service) freeze(id model.Identity, problems []model.Problem, seed uint64) ([]model.FrozenProblem, [][]environment.Action, error) {
	frozen := make([]model.FrozenProblem, 0, len(problems))
	setups := make([][]environment.Action, 0, len(problems))
	owner := map[string]string{}
	for _, p := range problems {
		f, err := problem.Freeze(p, seed)
		if err != nil {
			return nil, nil, fmt.Errorf("session: freeze %q: %w", p.ID, err)
		}
		f.Context = id.Context
		for _, ns := range append([]string{f.Namespace}, f.Definition.Scope.Namespaces...) {
			if other, taken := owner[ns]; taken {
				return nil, nil, fmt.Errorf("%w: %q and %q both use namespace %q with this seed", ErrNamespaceClash, other, p.ID, ns)
			}
			owner[ns] = p.ID
		}
		actions, err := author.BuildActions(p.Files, f.Parameters, p.Setup)
		if err != nil {
			return nil, nil, fmt.Errorf("session: render the setup of %q: %w", p.ID, err)
		}
		frozen = append(frozen, f)
		setups = append(setups, actions)
	}
	return frozen, setups, nil
}

// verifyFailure is an initial state that is not the declared one.
type verifyFailure struct {
	problemID string
	platform  bool // the grader could not judge (ERROR), as opposed to a different state
	count     int
	detail    string
}

func (v *verifyFailure) Error() string { return v.public().Error() }

// public is the learner-facing message: it names the problem and the kind of
// failure but none of its checks.
func (v *verifyFailure) public() error {
	if v.platform {
		return fmt.Errorf("problem %q: the initial state could not be judged (%d result(s) were ERROR), so the platform cannot vouch for it", v.problemID, v.count)
	}
	return fmt.Errorf("problem %q: the initial state differs from the declared starting state in %d check(s)", v.problemID, v.count)
}

// verifyInitialStates evaluates every problem right after setup, with its
// provided files in a private scratch workspace (what the learner will have
// after Start, without anything becoming visible before it), and compares the
// result with the declared starting state.
func (s *Service) verifyInitialStates(ctx context.Context, id model.Identity, rec model.SessionRecord) error {
	scratch := filepath.Join(s.cfg.StateDir, "prepare", rec.ID)
	defer os.RemoveAll(scratch)
	for _, f := range rec.Problems {
		dir := filepath.Join(scratch, f.Definition.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("prepare the scratch workspace of %q: %w", f.Definition.ID, err)
		}
		if err := writeFiles(dir, f.Assets, 0o600); err != nil {
			return fmt.Errorf("prepare the scratch workspace of %q: %w", f.Definition.ID, err)
		}
		ev, err := s.cfg.Engine.Evaluate(ctx, grader.Target{Identity: id, Problem: f, Workspace: dir})
		if err != nil {
			return fmt.Errorf("evaluate the initial state of %q: %w", f.Definition.ID, err)
		}
		if vf := compareInitial(f.Definition, ev); vf != nil {
			return vf
		}
	}
	return nil
}

// compareInitial returns nil when ev is exactly the starting state p declares.
func compareInitial(p model.Problem, ev model.Evaluation) *verifyFailure {
	var platform, differs []string
	got := map[string]model.CheckResult{}
	for _, r := range ev.Results {
		got[r.CriterionID] = r
		if r.Status == model.StatusError {
			platform = append(platform, fmt.Sprintf("criterion %q graded ERROR: %s", r.CriterionID, r.Reason))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.InitialExpected)) {
		r, ok := got[name]
		switch {
		case !ok:
			differs = append(differs, fmt.Sprintf("criterion %q was not graded", name))
		case r.Status == model.StatusError:
		case r.Status != p.InitialExpected[name]:
			differs = append(differs, fmt.Sprintf("criterion %q: got %s, want %s (%s)", name, r.Status, p.InitialExpected[name], r.Reason))
		}
	}
	switch {
	case len(platform) > 0:
	case ev.Score == nil:
		platform = append(platform, "the score is pending")
	case *ev.Score != p.InitialScore:
		differs = append(differs, fmt.Sprintf("score %d, want %d", *ev.Score, p.InitialScore))
	}
	switch {
	case len(platform) > 0:
		return &verifyFailure{problemID: p.ID, platform: true, count: len(platform), detail: p.ID + ": " + strings.Join(platform, "; ")}
	case len(differs) > 0:
		return &verifyFailure{problemID: p.ID, count: len(differs), detail: p.ID + ": " + strings.Join(differs, "; ")}
	}
	return nil
}

// setupError is the failure Prepare returns once the session exists: a
// message that reveals nothing private, the sentinel and the cause.
type setupError struct {
	msg   string
	cause []error
}

func (e *setupError) Error() string   { return e.msg }
func (e *setupError) Unwrap() []error { return append([]error{ErrSetupFailed}, e.cause...) }

// fail ends a preparation that reached the store: the operation is marked
// failed, the session becomes SETUP_ERROR (its inventory stays as the
// Manager recorded it; nothing is deleted here), and the details go to a
// private diagnostics file instead of the error, which must not reveal the
// problem's checks before the exam starts.
func (s *Service) fail(rec model.SessionRecord, op model.Operation, public error, detail string, cause ...error) (model.SessionRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), failureBudget)
	defer cancel()

	var notes []string
	op.Phase = PhaseFailed
	if err := s.cfg.Store.PutOperation(ctx, op); err != nil {
		notes = append(notes, fmt.Sprintf("the operation could not be marked failed: %v", err))
	}
	if err := s.cfg.Store.Transition(ctx, rec.ID, model.StatePreparing, model.StateSetupError); err != nil {
		notes = append(notes, fmt.Sprintf("the session could not be marked SETUP_ERROR: %v", err))
	}
	rec.State = model.StateSetupError

	msg := public.Error()
	if detail != "" {
		if path, err := s.writeDiagnostics(rec.ID, detail); err == nil {
			msg += "; details: " + path
		} else {
			notes = append(notes, fmt.Sprintf("the diagnostics could not be written: %v", err))
		}
	}
	msg += fmt.Sprintf("; session %s is SETUP_ERROR and its resources are kept for recovery", rec.ID)
	if len(notes) > 0 {
		msg += " (" + strings.Join(notes, "; ") + ")"
	}
	return hidePublic(rec), &setupError{msg: msg, cause: append([]error{public}, cause...)}
}

func (s *Service) writeDiagnostics(sessionID, detail string) (string, error) {
	dir := filepath.Join(s.cfg.StateDir, "diagnostics")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, sessionID+"-prepare.txt")
	return path, os.WriteFile(path, []byte(detail+"\n"), 0o600)
}

// hidePublic strips the public content (the English questions and provided
// files) from a record that has not been started, so that nothing handed to
// the caller can show a question early.
func hidePublic(rec model.SessionRecord) model.SessionRecord {
	rec.Problems = slices.Clone(rec.Problems)
	for i := range rec.Problems {
		rec.Problems[i].Question = ""
		rec.Problems[i].Assets = nil
	}
	return rec
}
