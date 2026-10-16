//go:build linux

// Package session runs the learner-facing session flow (spec 9.1): it
// prepares a session from validated problems without revealing a single
// question, starts the wall clock only when the learner confirms, and reports
// the session's time.
//
// Prepare checks that the dedicated environment is the one JunKloud created
// and has every capability the chosen problems need, refuses any problem
// that has no current validation certificate, freezes each problem with the
// session seed, stores the private package content under its hash, applies
// every problem's setup and compares each initial evaluation with the
// problem's declared starting state (an intentionally broken app is the
// expected result, not a platform fault). Start then publishes the English
// questions and provided files into a new attempt directory of the learner
// workspace and stores the start time. Both take the environment lock
// (author.LockPath), the same one validation, reset and grading take.
//
// Layout of the files this package writes:
//
//	<stateDir>/packages/<sha256 hex>/definition.json, files/...   private package, keyed by content hash
//	<stateDir>/prepare/<session>/...                              scratch for the initial evaluation, removed after
//	<stateDir>/diagnostics/<session>-prepare.txt                  why a preparation failed (private)
//	<workspace>/<session>/<problem>/attempt-<n>/                  question.md and provided files, written by Start
package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

// Errors. Match them with errors.Is.
var (
	// ErrInvalidOptions: the StartOptions do not describe a session.
	ErrInvalidOptions = errors.New("session: invalid start options")
	// ErrRetryUnavailable: retry sessions are composed from graded results,
	// which a later task provides.
	ErrRetryUnavailable = errors.New("session: retry sessions are not available yet")
	// ErrSessionActive: another session of the environment is not finished.
	ErrSessionActive = errors.New("session: another session is still active on this environment")
	// ErrNotCertified: a chosen problem has no current validation
	// certificate for this grader and environment. Nothing is replaced or
	// skipped silently.
	ErrNotCertified = errors.New("session: a problem has no current validation certificate")
	// ErrCapabilityUnavailable: a chosen problem needs an environment
	// capability that the doctor does not report as PASS.
	ErrCapabilityUnavailable = errors.New("session: a required environment capability is unavailable")
	// ErrReservedAsset: a chosen problem provides a file whose name the
	// session reserves for the question.
	ErrReservedAsset = errors.New("session: a problem provides a file named like the question")
	// ErrNamespaceClash: two chosen problems would own the same namespace.
	ErrNamespaceClash = errors.New("session: two problems would use the same namespace")
	// ErrEnvironmentChanged: the live environment is not the one this
	// service was composed for.
	ErrEnvironmentChanged = errors.New("session: the environment is not the one this service was composed for")
	// ErrSetupFailed: the preparation failed after the session was recorded.
	// The session is SETUP_ERROR and its inventory is kept for recovery.
	ErrSetupFailed = errors.New("session: preparation failed")
	// ErrNotPrepared: Start found a session whose preparation did not finish.
	ErrNotPrepared = errors.New("session: the session is not fully prepared")
	// ErrAlreadyStarted: the session's clock is already running (or it has
	// moved on).
	ErrAlreadyStarted = errors.New("session: the session is already started")
)

// Operation kind and phases of a preparation, recorded in the store so an
// interrupted preparation is detected and recovered (spec 11). A session is
// startable only when its operation reached PhaseDone.
const (
	OperationKind = "session-prepare"

	PhasePlanned      = "planned"       // intent recorded, nothing applied yet
	PhaseSetupApplied = "setup-applied" // every setup applied
	PhaseDone         = "done"          // initial states verified
	PhaseFailed       = "failed"        // the session is SETUP_ERROR
)

// OperationID is the ID of a session's preparation operation.
func OperationID(sessionID string) string { return "prepare-" + sessionID }

// StartOptions chooses the session to prepare. Mode is practice or mock
// (retry is reserved). Practice takes Count (5-20) problems matching the
// optional Domain, Difficulty and Tag. Mock names an exam file (MockID) and
// takes no filters. Seed zero means "pick one"; the seed used is stored in
// the record.
type StartOptions struct {
	Mode, MockID, Domain, Difficulty, Tag string
	Count                                 int
	Seed                                  uint64
	RetrySessionID                        string
	NewSeed                               bool
}

// Exclusion is a problem the request was about but which cannot be served,
// and why. Practice sessions are drawn only from servable problems; the
// exclusions say what was left out (spec 7.2: nothing is skipped silently).
type Exclusion struct {
	ProblemID string
	Reason    string
}

// Report is what Prepare found besides the session itself.
type Report struct {
	// Excluded lists the problems matching the request that were not
	// candidates: no current validation certificate, a required capability
	// that is not PASS, or a reserved provided-file name.
	Excluded []Exclusion
}

// Status is a session and its clock.
type Status struct {
	Session model.SessionRecord
	// Elapsed is the wall-clock time since the session started (zero before
	// the start). Overtime is Elapsed beyond the session's DurationLimit
	// (zero without a limit: practice sessions have none).
	Elapsed, Overtime time.Duration
	// ClockAnomaly is true when now is before the start time, i.e. the clock
	// moved backwards. Elapsed is then zero rather than negative, and the
	// result must say so.
	ClockAnomaly bool
}

// Environment is the part of *environment.Manager a Service drives.
type Environment interface {
	Ensure(ctx context.Context) (model.Identity, error)
	Check(ctx context.Context, id model.Identity) error
	Doctor(ctx context.Context, id model.Identity) (environment.DoctorReport, error)
	Apply(ctx context.Context, id model.Identity, owner environment.Owner, actions []environment.Action) (model.Inventory, error)
}

// Evaluator grades a frozen problem; *grader.Engine, the same engine
// validation and grading use.
type Evaluator interface {
	Evaluate(ctx context.Context, t grader.Target) (model.Evaluation, error)
}

// Records is the part of *store.Store a Service uses.
type Records interface {
	ActiveSessions(ctx context.Context, environmentID string) ([]store.SessionStatus, error)
	CreateSession(ctx context.Context, rec model.SessionRecord) error
	LoadSession(ctx context.Context, id string) (model.SessionRecord, error)
	Transition(ctx context.Context, id, from, to string) error
	MarkStarted(ctx context.Context, id string, at time.Time) error
	PutOperation(ctx context.Context, op model.Operation) error
	GetOperation(ctx context.Context, id string) (model.Operation, error)
	GetValidation(ctx context.Context, problemHash, graderVersion, environmentHash string) (model.ValidationRecord, error)
}

var (
	_ Environment = (*environment.Manager)(nil)
	_ Evaluator   = (*grader.Engine)(nil)
	_ Records     = (*store.Store)(nil)
)

// Config wires a Service. New builds one from the composed runtime;
// NewService takes it directly (unit tests use fakes for the cluster).
type Config struct {
	Store  Records
	Env    Environment
	Engine Evaluator
	// Identity is the verified environment the Engine was composed for.
	Identity model.Identity
	// GraderVersion is grader.GraderVersion.
	GraderVersion string
	// EnvironmentManifest computes the environment version manifest of a
	// verified identity (GraderConfig.EnvironmentManifest); it must use the
	// same GraderConfig.Images as the validation. Its hash
	// (author.EnvironmentHash) is the certificate's environment component,
	// and the manifest, hash and grader version are frozen on the session.
	EnvironmentManifest func(ctx context.Context, id model.Identity) (map[string]string, error)
	// PrepareEnvironment installs the platform and preloads the pinned images
	// (author.PrepareEnvironment); called under the lock.
	PrepareEnvironment func(ctx context.Context, id model.Identity) error
	// BankDir holds problems/<id>/ packages and exams/<id>.yaml files.
	BankDir string
	// StateDir is the installation's state directory.
	StateDir string
	// Workspace is the learner workspace root that Start writes attempt
	// directories into. Default: <StateDir>/workspace.
	Workspace string
	// Now is the clock (default time.Now). NewSessionID and NewSeed default
	// to random values.
	Now          func() time.Time
	NewSessionID func(mode string) (string, error)
	NewSeed      func() (uint64, error)
}

// Service prepares and starts sessions.
type Service struct{ cfg Config }

// Option adjusts the Config New builds.
type Option func(*Config)

// WithWorkspace sets the learner workspace root.
func WithWorkspace(dir string) Option { return func(c *Config) { c.Workspace = dir } }

// WithClock sets the clock used for operation timestamps.
func WithClock(now func() time.Time) Option { return func(c *Config) { c.Now = now } }

// New composes a Service on the real Manager and Store and on the runtime
// that author.NewRuntime built (its grader engine and verified identity), so
// sessions grade with exactly the composition validation used. graderCfg must
// be the GraderConfig the runtime was built with: its Images complete the
// environment hash that certificates are compared with. bankDir holds
// problems/ and exams/; stateDir is the installation's state directory.
func New(s *store.Store, env *environment.Manager, rt *author.Runtime, graderCfg author.GraderConfig, bankDir, stateDir string, opts ...Option) *Service {
	cfg := Config{
		Store: s, Env: env, Engine: rt.Grader.Engine, Identity: rt.Identity,
		GraderVersion:       grader.GraderVersion,
		EnvironmentManifest: graderCfg.EnvironmentManifest,
		PrepareEnvironment: func(ctx context.Context, id model.Identity) error {
			return author.PrepareEnvironment(ctx, env, id, graderCfg.Images)
		},
		BankDir: bankDir, StateDir: stateDir,
	}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := NewService(cfg)
	if err != nil {
		panic(err) // a programming error: the arguments above are always set
	}
	return svc
}

// NewService checks that cfg is complete and fills the defaults.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil, cfg.Env == nil, cfg.Engine == nil:
		return nil, errors.New("session: a service needs a Store, Env and Engine")
	case cfg.Identity.ID == "":
		return nil, errors.New("session: a service needs the verified environment identity")
	case cfg.GraderVersion == "", cfg.EnvironmentManifest == nil, cfg.PrepareEnvironment == nil:
		return nil, errors.New("session: a service needs the grader version, the environment manifest and the environment preparation")
	case cfg.BankDir == "", cfg.StateDir == "":
		return nil, errors.New("session: a service needs the bank and state directories")
	}
	if cfg.Workspace == "" {
		cfg.Workspace = filepath.Join(cfg.StateDir, "workspace")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewSessionID == nil {
		cfg.NewSessionID = randomSessionID
	}
	if cfg.NewSeed == nil {
		cfg.NewSeed = randomSeed
	}
	return &Service{cfg: cfg}, nil
}

func randomSessionID(mode string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: generate an ID: %w", err)
	}
	return mode + "-" + hex.EncodeToString(b[:]), nil
}

// randomSeed returns a non-zero random seed (zero means "unspecified").
func randomSeed() (uint64, error) {
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("session: generate a seed: %w", err)
		}
		if seed := binary.BigEndian.Uint64(b[:]); seed != 0 {
			return seed, nil
		}
	}
}
