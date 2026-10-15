//go:build linux

package environment

import (
	"errors"
	"testing"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func recordedIdentity() model.Identity {
	return model.Identity{
		ID:           "0123456789ab",
		ClusterName:  "junkloud-0123456789ab",
		Context:      "junkloud-0123456789ab",
		Endpoint:     "https://172.18.0.2:6443",
		CAHash:       "sha256:aaaa",
		MarkerUID:    "11111111-2222-3333-4444-555555555555",
		ContainerIDs: []string{"c0ffee"},
		Versions:     map[string]string{"kind": KindVersion},
	}
}

// TestRejectForeignIdentity: an identity that shares the recorded name but
// differs in CA, marker UID, container IDs or environment ID is rejected
// before any external command or API call is made, so nothing can reach the
// foreign target.
func TestRejectForeignIdentity(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	testutil.NoError(t, m.writeIdentity(recordedIdentity()))

	foreign := map[string]func(*model.Identity){
		"ca":         func(id *model.Identity) { id.CAHash = "sha256:bbbb" },
		"marker":     func(id *model.Identity) { id.MarkerUID = "99999999-2222-3333-4444-555555555555" },
		"containers": func(id *model.Identity) { id.ContainerIDs = []string{"deadbeef"} },
		"extra-node": func(id *model.Identity) { id.ContainerIDs = append(id.ContainerIDs, "deadbeef") },
		"id":         func(id *model.Identity) { id.ID = "ba9876543210" },
		"context":    func(id *model.Identity) { id.Context = "kind-junkloud-0123456789ab" },
		"empty":      func(id *model.Identity) { *id = model.Identity{ClusterName: id.ClusterName} },
	}
	for name, mutate := range foreign {
		t.Run(name, func(t *testing.T) {
			foreignIdentity := recordedIdentity()
			mutate(&foreignIdentity)
			testutil.Equal(t, foreignIdentity.ClusterName, recordedIdentity().ClusterName)

			err := m.Check(bg, foreignIdentity)
			testutil.ErrorIs(t, err, ErrIdentityMismatch)

			owner := Owner{
				Inventory: model.Inventory{EnvironmentID: foreignIdentity.ID, SessionID: "s1", ProblemID: "p1"},
				Namespace: "svc-alpha",
			}
			actions := []Action{{Kind: model.ActionApply, Objects: []map[string]any{configMap("", "cm")}}}
			inv, err := m.Apply(bg, foreignIdentity, owner, actions)
			testutil.ErrorIs(t, err, ErrIdentityMismatch)
			testutil.Equal(t, len(inv.Resources), 0)

			foreignClusterMutationCount := len(fake.Calls())
			testutil.Equal(t, foreignClusterMutationCount, 0)
		})
	}
}

func TestCheckWithoutRecordedEnvironment(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	testutil.ErrorIs(t, m.Check(bg, recordedIdentity()), ErrNoEnvironment)
	testutil.Equal(t, len(fake.Calls()), 0)
}

func TestIdentityDiffIgnoresEndpointAndOrder(t *testing.T) {
	a := recordedIdentity()
	a.ContainerIDs = []string{"x", "y"}
	b := a
	b.ContainerIDs = []string{"y", "x"}
	b.Endpoint = "https://172.18.0.9:6443"
	b.Versions = nil
	testutil.Equal(t, identityDiff(a, b), "")
	b.MarkerUID = "other"
	testutil.Equal(t, identityDiff(a, b), "marker_uid")
}

func TestIdentityFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, openTestStore(t, dir), WithRunner(&fakeRunner{}))
	_, err := m.readIdentity()
	testutil.ErrorIs(t, err, ErrNoEnvironment)
	testutil.NoError(t, m.writeIdentity(recordedIdentity()))
	got, err := m.readIdentity()
	testutil.NoError(t, err)
	testutil.Equal(t, got, recordedIdentity())
}

// TestRecoverCreateIntent: Ensure records the create intent before running
// kind; after an interruption a later Ensure resumes that same intent (same
// environment ID and cluster name), re-checks the real state, never creates
// a second cluster over existing nodes, and never deletes anything.
func TestRecoverCreateIntent(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	interrupted := errors.New("injected interruption")

	// First run: the intent is stored, then kind is "interrupted".
	first := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		if exe == "docker" && args[0] == "ps" {
			return executil.Result{}, nil // no nodes yet
		}
		if exe == "kind" && args[0] == "create" {
			return executil.Result{ExitCode: -1}, interrupted
		}
		return executil.Result{}, errors.New("unexpected command")
	}}
	_, err := New(dir, st, WithRunner(first)).Ensure(bg)
	testutil.ErrorIs(t, err, interrupted)
	ops, err := st.ListOperations(bg, OperationCreate)
	testutil.NoError(t, err)
	testutil.Equal(t, len(ops), 1)
	planned := ops[0]
	testutil.Equal(t, planned.Phase, PhasePlanned)
	testutil.Equal(t, planned.Planned.EnvironmentID, planned.EnvironmentID)
	testutil.Equal(t, first.count("kind create cluster --name "+ClusterName(planned.EnvironmentID)), 1)

	// Second run: nodes for the planned cluster exist but are not usable
	// (kind was cut off mid-create). Recovery must stop and report, not
	// create another cluster and not delete the half-made one.
	second := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		switch {
		case exe == "docker" && args[0] == "ps":
			return executil.Result{Stdout: []byte("abc123\n")}, nil
		case exe == "kind" && args[0] == "get":
			return executil.Result{ExitCode: 1}, executil.ErrExit
		}
		return executil.Result{}, errors.New("unexpected command")
	}}
	_, err = New(dir, st, WithRunner(second)).Ensure(bg)
	testutil.ErrorIs(t, err, ErrRecoveryNeeded)
	testutil.Equal(t, second.count("kind create"), 0)
	testutil.Equal(t, second.count("kind delete"), 0)
	testutil.Equal(t, second.count("docker rm"), 0)
	testutil.Equal(t, second.count("kind get kubeconfig --internal --name "+ClusterName(planned.EnvironmentID)), 1)

	// Third run: nothing real exists for the intent, so the same planned
	// cluster (not a new environment ID) is created.
	third := &fakeRunner{respond: first.respond}
	_, err = New(dir, st, WithRunner(third)).Ensure(bg)
	testutil.ErrorIs(t, err, interrupted)
	testutil.Equal(t, third.count("kind create cluster --name "+ClusterName(planned.EnvironmentID)), 1)
	ops, err = st.ListOperations(bg, OperationCreate)
	testutil.NoError(t, err)
	testutil.Equal(t, len(ops), 1)
}

// An unfinished intent recorded for another state directory is not adopted.
func TestRecoverIgnoresOtherStateDir(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	other := New(t.TempDir(), st, WithRunner(&fakeRunner{}))
	testutil.NoError(t, st.PutOperation(bg, other.plannedCreate("feedfacecafe")))

	fake := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		if exe == "docker" && args[0] == "ps" {
			return executil.Result{}, nil
		}
		return executil.Result{ExitCode: -1}, errors.New("stop")
	}}
	_, _ = New(dir, st, WithRunner(fake)).Ensure(bg)
	testutil.Equal(t, fake.count("kind create cluster --name "+ClusterName("feedfacecafe")), 0)
	testutil.Equal(t, fake.count("kind create cluster --name junkloud-"), 1)
}
