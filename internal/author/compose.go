//go:build linux

package author

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

const (
	inputKeyFile = "input.key"
	inputKeySize = 32
	lockFile     = "env.lock"
)

// LockPath is the environment's mutation lock file under stateDir. Start,
// reset, bank validation and grading all take it (internal/lock).
func LockPath(stateDir string) string { return filepath.Join(stateDir, lockFile) }

// InputKey returns the installation's secret key for input fingerprints
// (grader.WithInputKey), creating it on first use: 32 random bytes in
// <stateDir>/input.key, mode 0600, created atomically so two processes
// cannot end up with different keys. The key must stay stable, otherwise
// every earlier fingerprint would look like a changed answer; a damaged key
// file is therefore reported, never replaced. stateDir must exist.
func InputKey(stateDir string) ([]byte, error) {
	path := filepath.Join(stateDir, inputKeyFile)
	read := func() ([]byte, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(b) != inputKeySize {
			return nil, fmt.Errorf("author: %s has %d bytes, want %d: it is damaged; restore it from a backup instead of letting a new key invalidate every input fingerprint", path, len(b), inputKeySize)
		}
		return b, nil
	}
	if b, err := read(); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	key := make([]byte, inputKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("author: generate input key: %w", err)
	}
	tmp, err := os.CreateTemp(stateDir, ".input.key-*")
	if err != nil {
		return nil, fmt.Errorf("author: create input key: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(key)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("author: write input key: %w", werr)
	}
	// Link, unlike rename, never replaces a key another process just made.
	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return key, nil
	case errors.Is(err, fs.ErrExist):
		return read()
	default:
		return nil, fmt.Errorf("author: create input key: %w", err)
	}
}

// EnvironmentManifest is the environment's version manifest that
// EnvironmentHash digests: the versions recorded in the identity (kind, node
// image, Kubernetes server) and the pinned platform images (cloud-provider-
// kind, envoy, metrics-server, the probe image). It holds no identifier of
// one environment instance, so a rebuilt environment with the same versions
// keeps its certificates.
func EnvironmentManifest(id model.Identity) map[string]string {
	m := maps.Clone(id.Versions)
	if m == nil {
		m = map[string]string{}
	}
	m["cloud-provider-kind"] = environment.CloudProviderKindImage.Ref()
	m["envoy-proxy"] = environment.EnvoyProxyImage.Ref()
	m["metrics-server"] = environment.MetricsServerImage.Ref()
	m["probe-image"] = environment.ProbeImage.Ref()
	return m
}

// GraderConfig is what composing the runtime grader needs.
type GraderConfig struct {
	// StateDir is the installation's state directory (the Manager's and the
	// Store's); the input key lives in it.
	StateDir string
	// Images are the pinned images the problems use, in addition to the
	// probe image, which is always provided. They are preloaded
	// (PrepareEnvironment) and declared to the grader as provided, so a pull
	// failure of any other image can be the learner's wrong image.
	Images []environment.PinnedImage
	// Options are further grader options (clock, default timeout,
	// parallelism, poll interval). They go to the engine and to EVERY
	// checker, so one option decides every default timeout.
	Options []grader.Option
}

// ProvidedImages are the images the environment provides: the probe image
// and the problems' pinned images.
func (c GraderConfig) ProvidedImages() []environment.PinnedImage {
	return append([]environment.PinnedImage{environment.ProbeImage}, c.Images...)
}

// providedNames are the provided images' references as pods use them.
func (c GraderConfig) providedNames() []string {
	var names []string
	for _, img := range c.ProvidedImages() {
		names = append(names, img.Name)
	}
	return names
}

// The real implementations of the Validator's seams.
var (
	_ Applier   = (*environment.Manager)(nil)
	_ Evaluator = (*grader.Engine)(nil)
	_ Records   = (*store.Store)(nil)
	_ Janitor   = (*ClusterJanitor)(nil)
)

// Grader is the runtime grader: the engine with every checker kind
// registered. Sessions, exams and validation all grade with this one
// composition.
type Grader struct {
	Engine *grader.Engine
	// Artifacts is the checker for file, image, helm, kustomize, logs, job
	// and cronjob criteria; sessions also call its CaptureInputs.
	Artifacts *grader.ArtifactChecker
}

// Close releases the checkers' scratch space.
func (g *Grader) Close() error { return g.Artifacts.Close() }

// ComposeGrader builds the runtime grader for a verified environment id. It
// changes nothing in the cluster (PrepareEnvironment does, under the lock).
// The same option list reaches the engine and every checker: the provided
// image set (probe image plus problem images, fail-closed otherwise), the
// probe image and control namespace, the persisted input key, and cfg.Options.
func ComposeGrader(m *environment.Manager, id model.Identity, cfg GraderConfig) (*Grader, error) {
	key, err := InputKey(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	opts := append([]grader.Option{
		grader.WithRedactor(grader.NewRedactor()),
		grader.WithPreloadedImages(cfg.providedNames()...),
		grader.WithProbeImage(environment.ProbeImage.Name),
		grader.WithProbeControlNamespace(environment.PlatformNamespace),
		grader.WithInputKey(key),
	}, cfg.Options...)

	kubeconfig := m.KubeconfigPath()
	reader, err := grader.NewClusterReader(kubeconfig, id.Context)
	if err != nil {
		return nil, err
	}
	probes, err := grader.NewPodProbeRunner(kubeconfig, id.Context, opts...)
	if err != nil {
		return nil, err
	}
	artifacts, err := grader.NewClusterArtifactChecker(kubeconfig, id.Context, probes, opts...)
	if err != nil {
		return nil, err
	}
	security, err := grader.NewClusterSecurityChecker(kubeconfig, id.Context, opts...)
	if err != nil {
		artifacts.Close()
		return nil, err
	}
	checkers := grader.DefaultCheckers(reader, opts...)
	grader.AddNetworkCheckers(checkers, grader.NewNetworkChecker(reader, probes, opts...))
	grader.AddArtifactCheckers(checkers, artifacts)
	grader.AddSecurityCheckers(checkers, security)
	return &Grader{Engine: grader.NewEngine(checkers, opts...), Artifacts: artifacts}, nil
}

// PrepareEnvironment readies a verified environment for grading: it installs
// the platform (EnsurePlatform, right after Ensure) and preloads the probe
// image and the problems' pinned images, which the nodes cannot pull. The
// caller holds the environment lock.
func PrepareEnvironment(ctx context.Context, m *environment.Manager, id model.Identity, images []environment.PinnedImage) error {
	if err := m.EnsurePlatform(ctx, id); err != nil {
		return fmt.Errorf("author: install the platform: %w", err)
	}
	if err := m.PreloadImages(ctx, id, append([]environment.PinnedImage{environment.ProbeImage}, images...)...); err != nil {
		return fmt.Errorf("author: preload images: %w", err)
	}
	return nil
}

// Runtime is a Validator composed on a real environment.
type Runtime struct {
	Validator *Validator
	Grader    *Grader
	// EnvironmentHash is the certificate component of this environment
	// (EnvironmentHash(EnvironmentManifest(id))); sessions must compare
	// against the same value.
	EnvironmentHash string
}

// NewRuntime composes the Validator on the Manager, Store and grader. The
// Validator prepares the environment itself, under the lock, on every
// Validate.
func NewRuntime(m *environment.Manager, st *store.Store, id model.Identity, cfg GraderConfig, janitor ...JanitorOption) (*Runtime, error) {
	g, err := ComposeGrader(m, id, cfg)
	if err != nil {
		return nil, err
	}
	j, err := NewClusterJanitor(m.KubeconfigPath(), id.Context, janitor...)
	if err != nil {
		g.Close()
		return nil, err
	}
	envHash := EnvironmentHash(EnvironmentManifest(id))
	v, err := NewValidator(Config{
		Identity: id, Env: m, Janitor: j, Engine: g.Engine, Store: st,
		LockPath: LockPath(cfg.StateDir), GraderVersion: grader.GraderVersion, EnvironmentHash: envHash,
		ProvidedImages: cfg.providedNames(),
		Prepare:        func(ctx context.Context) error { return PrepareEnvironment(ctx, m, id, cfg.Images) },
	})
	if err != nil {
		g.Close()
		return nil, err
	}
	return &Runtime{Validator: v, Grader: g, EnvironmentHash: envHash}, nil
}

// Close releases the grader's scratch space.
func (r *Runtime) Close() error { return r.Grader.Close() }
