package author_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// loadFixture is the partial-credit fixture with each parameter pinned to the
// value seed 1 freezes, so one seed covers every allowed value (a validation
// must cover them all) and the tests stay about what they test.
// loadFullFixture keeps every allowed value.
func loadFixture(t *testing.T) model.Problem {
	t.Helper()
	p := loadFullFixture(t)
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	p.Parameters = map[string][]any{}
	for name, v := range f.Parameters {
		p.Parameters[name] = []any{v}
	}
	return p
}

func loadFullFixture(t *testing.T) model.Problem {
	t.Helper()
	p, err := problem.Load(filepath.Join("testdata", "partial-credit"))
	testutil.NoError(t, err)
	return p
}

func pointer(n int) *int { return &n }

func passedRecord(hash, graderVersion, envHash string) model.ValidationRecord {
	return model.ValidationRecord{
		ProblemHash: hash, GraderVersion: graderVersion, EnvironmentHash: envHash,
		Seeds:  []uint64{1},
		Passed: true,
		Cases: map[string]model.Evaluation{
			"initial": {Score: pointer(2), MaxScore: 10, Complete: true},
			"solved":  {Score: pointer(10), MaxScore: 10, Complete: true},
		},
	}
}

var manifest = map[string]string{
	"kind": "v0.33.0", "node_image": "kindest/node:v1.37.0@sha256:aa", "kubernetes": "v1.37.0",
	"probe-image": "docker.io/library/busybox:1.37.0@sha256:bb",
}

func TestEnvironmentHash(t *testing.T) {
	base := author.EnvironmentHash(manifest)
	if len(base) != len("sha256:")+64 || base[:7] != "sha256:" {
		t.Fatalf("hash %q is not sha256:<64 hex>", base)
	}
	// Deterministic and independent of map order.
	testutil.Equal(t, author.EnvironmentHash(map[string]string{
		"probe-image": manifest["probe-image"], "kubernetes": manifest["kubernetes"],
		"node_image": manifest["node_image"], "kind": manifest["kind"],
	}), base)
	// Every value, every key and every added or removed entry changes it,
	// and key/value boundaries cannot be shifted to collide.
	variants := []map[string]string{
		{"kind": "v0.34.0", "node_image": manifest["node_image"], "kubernetes": manifest["kubernetes"], "probe-image": manifest["probe-image"]},
		{"kind": manifest["kind"], "node_image": manifest["node_image"], "kubernetes": "v1.38.0", "probe-image": manifest["probe-image"]},
		{"kind": manifest["kind"], "node_image": manifest["node_image"], "kubernetes": manifest["kubernetes"], "probe-image": "docker.io/library/busybox:1.37.0@sha256:cc"},
		{"kind": manifest["kind"], "node_image": manifest["node_image"], "kubernetes": manifest["kubernetes"]},
		{"kind": manifest["kind"], "node_image": manifest["node_image"], "kubernetes": manifest["kubernetes"], "probe-image": manifest["probe-image"], "extra": "1"},
		{"kind": manifest["kind"], "node_image": manifest["node_image"], "kubernetes": manifest["kubernetes"], "probe_image": manifest["probe-image"]},
		{"kind=v0.33.0\nnode_image": "kindest/node:v1.37.0@sha256:aa", "kubernetes": manifest["kubernetes"], "probe-image": manifest["probe-image"]},
	}
	seen := map[string]bool{base: true}
	for i, v := range variants {
		h := author.EnvironmentHash(v)
		if seen[h] {
			t.Errorf("variant %d collides with another manifest: %v", i, v)
		}
		seen[h] = true
	}
	// An empty manifest still hashes (and differs from any real one).
	if author.EnvironmentHash(nil) == base {
		t.Fatal("empty manifest equals a real one")
	}
}

func TestStaleCertificate(t *testing.T) {
	p := loadFixture(t)
	hash, err := problem.Hash(p)
	testutil.NoError(t, err)
	envHash := author.EnvironmentHash(manifest)
	engineVersion := grader.GraderVersion
	record := passedRecord(hash, engineVersion, envHash)

	testutil.Equal(t, record.Cases["initial"].MaxScore, 10)
	testutil.Equal(t, *record.Cases["initial"].Score, 2)
	testutil.Equal(t, author.CertificateMatches(record, hash, engineVersion, envHash), true)

	// Any change to the problem package changes its hash: here the reference
	// answer, a private file the learner never sees.
	changed := p
	changed.Files = map[string][]byte{}
	for k, v := range p.Files {
		changed.Files[k] = v
	}
	changed.Files["solution/web.yaml"] = append(append([]byte{}, p.Files["solution/web.yaml"]...), "# edited\n"...)
	changedHash, err := problem.Hash(changed)
	testutil.NoError(t, err)
	if changedHash == hash {
		t.Fatal("editing a package file did not change the problem hash")
	}
	testutil.Equal(t, author.CertificateMatches(record, changedHash, engineVersion, envHash), false)

	// Grader semantics changed.
	testutil.Equal(t, author.CertificateMatches(record, hash, "grader-999", envHash), false)
	// Environment versions changed (a different kind release, a new node image).
	newer := map[string]string{}
	for k, v := range manifest {
		newer[k] = v
	}
	newer["kind"] = "v0.34.0"
	testutil.Equal(t, author.CertificateMatches(record, hash, engineVersion, author.EnvironmentHash(newer)), false)
	// A failed validation never certifies, even for a matching triple.
	failed := record
	failed.Passed = false
	testutil.Equal(t, author.CertificateMatches(failed, hash, engineVersion, envHash), false)
	// Empty values never match.
	testutil.Equal(t, author.CertificateMatches(model.ValidationRecord{Passed: true}, "", "", ""), false)
}

type memCertificates struct{ s *store.Store }

func (m memCertificates) GetValidation(ctx context.Context, ph, gv, eh string) (model.ValidationRecord, error) {
	return m.s.GetValidation(ctx, ph, gv, eh)
}

// The stored certificate is looked up by the current triple, so a changed
// component finds nothing and the problem needs a new validation.
func TestCurrentCertificate(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "junkloud.db"))
	testutil.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	envHash := author.EnvironmentHash(manifest)

	rec := passedRecord("sha256:aaa", grader.GraderVersion, envHash)
	testutil.NoError(t, s.SaveValidation(ctx, rec))

	got, ok, err := author.CurrentCertificate(ctx, memCertificates{s}, "sha256:aaa", grader.GraderVersion, envHash)
	testutil.NoError(t, err)
	testutil.Equal(t, ok, true)
	testutil.Equal(t, got.Seeds, []uint64{1})

	for name, triple := range map[string][3]string{
		"problem changed":     {"sha256:bbb", grader.GraderVersion, envHash},
		"grader changed":      {"sha256:aaa", "grader-99", envHash},
		"environment changed": {"sha256:aaa", grader.GraderVersion, author.EnvironmentHash(nil)},
	} {
		_, ok, err := author.CurrentCertificate(ctx, memCertificates{s}, triple[0], triple[1], triple[2])
		if err != nil || ok {
			t.Errorf("%s: ok=%v err=%v, want a clean miss", name, ok, err)
		}
	}

	// The newest record wins: a later failing validation of the same triple
	// withdraws the certificate.
	failing := rec
	failing.Passed = false
	testutil.NoError(t, s.SaveValidation(ctx, failing))
	_, ok, err = author.CurrentCertificate(ctx, memCertificates{s}, "sha256:aaa", grader.GraderVersion, envHash)
	testutil.NoError(t, err)
	testutil.Equal(t, ok, false)
}

func TestVolumeSemanticsInvalidateImageCertificates(t *testing.T) {
	record := passedRecord("sha256:volume", "grader-3", "env")
	if author.CertificateMatches(record, "sha256:volume", grader.GraderVersion, "env") {
		t.Fatal("pre-volume grader certificate is still current")
	}
}

func TestCronSuccessSemanticsInvalidateVolumeCertificates(t *testing.T) {
	record := passedRecord("sha256:batch", "grader-4", "env")
	if author.CertificateMatches(record, "sha256:batch", grader.GraderVersion, "env") {
		t.Fatal("pre-scheduled-success grader certificate is still current")
	}
}

func TestLogBehaviorSemanticsInvalidateScheduleCertificates(t *testing.T) {
	record := passedRecord("sha256:logs", "grader-5", "env")
	if author.CertificateMatches(record, "sha256:logs", grader.GraderVersion, "env") {
		t.Fatal("pre-shared-file/node behavior certificate is still current")
	}
}
