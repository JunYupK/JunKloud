//go:build linux

package author_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestInputKey(t *testing.T) {
	dir := t.TempDir()
	key, err := author.InputKey(dir)
	testutil.NoError(t, err)
	testutil.Equal(t, len(key), 32)
	if bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("key is all zero")
	}
	info, err := os.Stat(filepath.Join(dir, "input.key"))
	testutil.NoError(t, err)
	testutil.Equal(t, info.Mode().Perm(), os.FileMode(0o600))

	// Stable across calls (a changed key would make every earlier input
	// fingerprint look like a changed answer).
	again, err := author.InputKey(dir)
	testutil.NoError(t, err)
	testutil.Equal(t, again, key)

	// Distinct installations get distinct keys.
	other, err := author.InputKey(t.TempDir())
	testutil.NoError(t, err)
	if bytes.Equal(other, key) {
		t.Fatal("two state directories share a key")
	}

	// A damaged key is reported, never silently replaced.
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "input.key"), []byte("short"), 0o600))
	if _, err := author.InputKey(dir); err == nil {
		t.Fatal("accepted a truncated key")
	}
	data, err := os.ReadFile(filepath.Join(dir, "input.key"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(data), "short")

	if _, err := author.InputKey(filepath.Join(dir, "missing", "deeper")); err == nil {
		t.Fatal("created a key in a state directory that does not exist")
	}
}

func TestEnvironmentManifest(t *testing.T) {
	a := model.Identity{
		ID: "0123456789ab", ClusterName: "junkloud-0123456789ab", MarkerUID: "uid-a", CAHash: "ca-a", Endpoint: "https://10.0.0.2:6443",
		Versions: map[string]string{"kind": "v0.33.0", "node_image": "kindest/node:v1.37.0", "kubernetes": "v1.37.0"},
	}
	m := author.EnvironmentManifest(a)
	for key, want := range map[string]string{
		"kind": "v0.33.0", "kubernetes": "v1.37.0",
		"probe-image":         environment.ProbeImage.Ref(),
		"cloud-provider-kind": environment.CloudProviderKindImage.Ref(),
		"envoy-proxy":         environment.EnvoyProxyImage.Ref(),
		"metrics-server":      environment.MetricsServerImage.Ref(),
	} {
		testutil.Equal(t, m[key], want)
	}
	// Nothing that identifies one particular environment is part of it.
	for _, forbidden := range []string{"0123456789ab", "uid-a", "ca-a", "10.0.0.2"} {
		for k, v := range m {
			if k == forbidden || v == forbidden {
				t.Errorf("manifest contains environment identity %q (%s=%s)", forbidden, k, v)
			}
		}
	}
	// A rebuilt environment with the same versions keeps its certificates.
	b := a
	b.ID, b.ClusterName, b.MarkerUID, b.CAHash, b.Endpoint = "ba9876543210", "junkloud-ba9876543210", "uid-b", "ca-b", "https://10.0.0.9:6443"
	testutil.Equal(t, author.EnvironmentHash(author.EnvironmentManifest(b)), author.EnvironmentHash(m))

	// A changed Kubernetes version or node image makes them stale.
	for _, mutate := range []func(model.Identity) model.Identity{
		func(id model.Identity) model.Identity {
			id.Versions = cloneVersions(id.Versions, "kubernetes", "v1.38.0")
			return id
		},
		func(id model.Identity) model.Identity {
			id.Versions = cloneVersions(id.Versions, "node_image", "kindest/node:v1.38.0")
			return id
		},
		func(id model.Identity) model.Identity {
			id.Versions = cloneVersions(id.Versions, "kind", "v0.34.0")
			return id
		},
	} {
		if author.EnvironmentHash(author.EnvironmentManifest(mutate(a))) == author.EnvironmentHash(m) {
			t.Error("a version change did not change the environment hash")
		}
	}
	// The input's map is not modified.
	testutil.Equal(t, len(a.Versions), 3)
}

func cloneVersions(in map[string]string, key, value string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}
