//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

const ResetOperationKind = "session-reset"

var ErrResetPending = errors.New("session: reset is unfinished; run ckad reset --recover")
var ErrResetConfirmation = errors.New("session: full reset requires confirmation of the environment ID")

type ResetOptions struct {
	ProblemID              string
	Cluster                bool
	ConfirmedEnvironmentID string
}
type cleaner interface {
	Cleanup(context.Context, model.Identity, model.Inventory) error
}
type inventoryRecords interface {
	GetInventory(context.Context, string, string, string) (model.Inventory, error)
}

func (s *Service) Reset(ctx context.Context, sessionID string, opts ResetOptions) error {
	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if err != nil {
		return err
	}
	defer release()
	rec := model.SessionRecord{EnvironmentID: s.cfg.Identity.ID, State: model.StateAbandoned}
	if sessionID != "" {
		rec, err = s.cfg.Store.LoadSession(ctx, sessionID)
		if err != nil {
			return err
		}
	} else if !opts.Cluster {
		return ErrInvalidOptions
	}
	if rec.EnvironmentID != s.cfg.Identity.ID {
		return ErrEnvironmentChanged
	}
	if opts.Cluster && opts.ProblemID != "" {
		return ErrInvalidOptions
	}
	if opts.ProblemID == "" && opts.ConfirmedEnvironmentID != rec.EnvironmentID {
		return ErrResetConfirmation
	}
	active, err := s.cfg.Store.ActiveSessions(ctx, rec.EnvironmentID)
	if err != nil {
		return err
	}
	for _, a := range active {
		if a.ID != rec.ID {
			return ErrSessionActive
		}
	}
	if opts.ProblemID != "" {
		if rec.Mode == model.ModeMock || rec.State != model.StateRunning {
			return fmt.Errorf("%w: individual reset requires running practice", ErrInvalidOptions)
		}
		if !slices.ContainsFunc(rec.Problems, func(f model.FrozenProblem) bool { return f.Definition.ID == opts.ProblemID }) {
			return ErrInvalidOptions
		}
		// Missing exact package refuses reset before deleting a single object.
		for _, f := range rec.Problems {
			if f.Definition.ID == opts.ProblemID {
				if _, err := LoadPackage(s.cfg.StateDir, f.Hash); err != nil {
					return err
				}
			}
		}
	}
	if opts.Cluster {
		return s.resetCluster(ctx, rec, opts)
	}
	if err = s.requireNoPendingReset(ctx); err != nil {
		return err
	}
	if err = s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return err
	}
	ops, err := s.cfg.Store.ListOperations(ctx, ResetOperationKind)
	if err != nil {
		return err
	}
	op := model.Operation{ID: fmt.Sprintf("reset-%s-%d", rec.ID, len(ops)+1), EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, Kind: ResetOperationKind, Phase: PhasePlanned, CreatedAt: s.cfg.Now().UTC(), Planned: model.Inventory{EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, ProblemID: opts.ProblemID}}
	if opts.ProblemID != "" {
		_, n, err := LatestAttempt(s.cfg.Workspace, rec.ID, opts.ProblemID)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		hist, err := s.loadHistory(ctx, rec.ID)
		if err != nil {
			return err
		}
		n = max(n, hist.currentAttempt(opts.ProblemID))
		for _, grading := range hist.ops {
			n = max(n, grading.Planned.Attempts[opts.ProblemID])
		}
		op.Planned.ArtifactPaths = []string{AttemptDir(s.cfg.Workspace, rec.ID, opts.ProblemID, n+1)}
	}
	if err = s.cfg.Store.PutOperation(ctx, op); err != nil {
		return err
	}
	return s.resumeReset(ctx, rec, op)
}

func (s *Service) requireNoPendingReset(ctx context.Context) error {
	for _, kind := range []string{ResetOperationKind, environment.OperationCleanup} {
		ops, err := s.cfg.Store.ListOperations(ctx, kind)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.EnvironmentID == s.cfg.Identity.ID && op.Phase != PhaseDone {
				return fmt.Errorf("%w: %s at %s", ErrResetPending, op.ID, op.Phase)
			}
		}
	}
	return nil
}

func (s *Service) problemInventory(ctx context.Context, rec model.SessionRecord, f model.FrozenProblem) (model.Inventory, error) {
	st, ok := s.cfg.Store.(inventoryRecords)
	if !ok {
		return model.Inventory{}, errors.New("session: inventory store unavailable")
	}
	inv, err := st.GetInventory(ctx, rec.EnvironmentID, rec.ID, f.Definition.ID)
	if errors.Is(err, store.ErrNotFound) {
		inv = model.Inventory{EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, ProblemID: f.Definition.ID}
	} else if err != nil {
		return inv, err
	}
	return environment.NamespaceInventory(inv, append([]string{f.Namespace}, f.Definition.Scope.Namespaces...), slices.Clone(f.Definition.Scope.ClusterResources)), nil
}

func (s *Service) resumeReset(ctx context.Context, rec model.SessionRecord, op model.Operation) error {
	c, ok := s.cfg.Env.(cleaner)
	if !ok {
		return errors.New("session: environment cleanup unavailable")
	}
	advance := func(phase string) error { op.Phase = phase; return s.cfg.Store.PutOperation(ctx, op) }
	selected := rec.Problems
	if op.Planned.ProblemID != "" {
		selected = nil
		for _, f := range rec.Problems {
			if f.Definition.ID == op.Planned.ProblemID {
				selected = append(selected, f)
			}
		}
	}
	if op.Phase == PhasePlanned {
		for _, f := range selected {
			inv, err := s.problemInventory(ctx, rec, f)
			if err != nil {
				return err
			}
			if err = c.Cleanup(ctx, s.cfg.Identity, inv); err != nil {
				return err
			}
		}
		if err := advance("deleted"); err != nil {
			return err
		}
	}
	if op.Planned.ProblemID == "" {
		if rec.State != model.StateAbandoned {
			if err := s.cfg.Store.Transition(ctx, rec.ID, rec.State, model.StateAbandoned); err != nil {
				return err
			}
		}
		return advance(PhaseDone)
	}
	if len(selected) != 1 {
		return ErrInvalidOptions
	}
	f := selected[0]
	if op.Phase == "deleted" || op.Phase == "rebuilding" {
		pkg, err := LoadPackage(s.cfg.StateDir, f.Hash)
		if err != nil {
			return err
		}
		actions, err := author.BuildActions(pkg.Files, f.Parameters, pkg.Setup)
		if err != nil {
			return err
		}
		if err = advance("rebuilding"); err != nil {
			return err
		}
		inv, err := s.problemInventory(ctx, rec, f)
		if err != nil {
			return err
		}
		_, err = s.cfg.Env.Apply(ctx, s.cfg.Identity, environment.Owner{Inventory: inv, Namespace: f.Namespace, Scope: f.Definition.Scope, CreateNamespaces: true}, actions)
		if err != nil {
			return err
		}
		if err = advance("setup-applied"); err != nil {
			return err
		}
	}
	if op.Phase == "setup-applied" {
		one := rec
		one.Problems = selected
		if err := s.verifyInitialStates(ctx, s.cfg.Identity, one); err != nil {
			return err
		}
		if err := advance("verified"); err != nil {
			return err
		}
	}
	if op.Phase == "verified" {
		if len(op.Planned.ArtifactPaths) != 1 {
			return errors.New("session: missing recorded attempt path")
		}
		dir := op.Planned.ArtifactPaths[0]
		if filepath.Dir(dir) != ProblemDir(s.cfg.Workspace, rec.ID, f.Definition.ID) {
			return errors.New("session: invalid recorded attempt path")
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		files := maps.Clone(f.Assets)
		if files == nil {
			files = map[string][]byte{}
		}
		files[QuestionFile] = []byte(f.Question)
		for name, data := range files {
			// Publishing may have stopped between files; accept only identical files,
			// never replace personal work that appeared in the meantime.
			target := filepath.Join(dir, filepath.FromSlash(name))
			if info, err := os.Lstat(target); err == nil {
				if !info.Mode().IsRegular() {
					return errors.New("session: unsafe existing attempt file")
				}
				old, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				if string(old) != string(data) {
					return fmt.Errorf("session: preserve changed attempt file %s", target)
				}
				continue
			}
			if err := writeFiles(dir, map[string][]byte{name: data}, 0644); err != nil {
				return err
			}
		}
		if err := advance("published"); err != nil {
			return err
		}
	}
	return advance(PhaseDone)
}

// Recover resumes only explicit reset intents. It never creates learner
// resources merely because an ordinary command noticed they were missing.
func (s *Service) Recover(ctx context.Context, environmentID string) error {
	if environmentID != s.cfg.Identity.ID {
		return ErrEnvironmentChanged
	}
	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if err != nil {
		return err
	}
	defer release()
	ops, err := s.cfg.Store.ListOperations(ctx, ResetOperationKind)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.EnvironmentID != environmentID || op.Phase == PhaseDone {
			continue
		}
		rec := model.SessionRecord{EnvironmentID: environmentID, State: model.StateAbandoned}
		if op.SessionID != "" {
			rec, err = s.cfg.Store.LoadSession(ctx, op.SessionID)
			if err != nil {
				return err
			}
		}
		if op.Planned.ProblemID == "!cluster" {
			if err = s.resumeClusterReset(ctx, rec, op); err != nil {
				return err
			}
			continue
		}
		if err = s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
			return err
		}
		if err = s.resumeReset(ctx, rec, op); err != nil {
			return err
		}
	}
	return s.requireNoPendingReset(ctx)
}
