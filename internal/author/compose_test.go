//go:build linux

package author_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

var okTools = map[string]string{
	"helm": "v4.3.0+gbec5b06", "kubectl": "v1.37.0", "docker-client": "29.8.2", "docker-server": "29.8.2",
}

var pinned = []environment.PinnedImage{
	{Name: "docker.io/library/nginx:1.29.1", Digest: "sha256:" + strings.Repeat("a", 64)},
	{Name: "docker.io/library/redis:8.0", Digest: "sha256:" + strings.Repeat("b", 64)},
}

func mustManifest(t *testing.T, id model.Identity, images []environment.PinnedImage, tools map[string]string) map[string]string {
	t.Helper()
	m, err := author.EnvironmentManifest(id, images, tools)
	testutil.NoError(t, err)
	return m
}

func TestEnvironmentManifest(t *testing.T) {
	a := model.Identity{
		ID: "0123456789ab", ClusterName: "junkloud-0123456789ab", MarkerUID: "uid-a", CAHash: "ca-a", Endpoint: "https://10.0.0.2:6443",
		Versions: map[string]string{"kind": "v0.33.0", "node_image": "kindest/node:v1.37.0", "kubernetes": "v1.37.0"},
	}
	m := mustManifest(t, a, pinned, okTools)
	for key, want := range map[string]string{
		"kind": "v0.33.0", "kubernetes": "v1.37.0",
		"probe-image":             environment.ProbeImage.Ref(),
		"cloud-provider-kind":     environment.CloudProviderKindImage.Ref(),
		"envoy-proxy":             environment.EnvoyProxyImage.Ref(),
		"metrics-server":          environment.MetricsServerImage.Ref(),
		"image:" + pinned[0].Name: pinned[0].Digest,
		"image:" + pinned[1].Name: pinned[1].Digest,
		"tool:helm":               "v4.3.0+gbec5b06",
		"tool:kubectl":            "v1.37.0",
		"tool:docker-client":      "29.8.2",
		"tool:docker-server":      "29.8.2",
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
	hash := author.EnvironmentHash(m)
	// A rebuilt environment with the same versions keeps its certificates.
	b := a
	b.ID, b.ClusterName, b.MarkerUID, b.CAHash, b.Endpoint = "ba9876543210", "junkloud-ba9876543210", "uid-b", "ca-b", "https://10.0.0.9:6443"
	testutil.Equal(t, author.EnvironmentHash(mustManifest(t, b, pinned, okTools)), hash)
	// The same inputs in another order are the same environment.
	testutil.Equal(t, author.EnvironmentHash(mustManifest(t, a, []environment.PinnedImage{pinned[1], pinned[0], pinned[1]}, okTools)), hash)

	// Anything that can change a verdict makes the certificates stale.
	changedDigest := []environment.PinnedImage{pinned[0], {Name: pinned[1].Name, Digest: "sha256:" + strings.Repeat("c", 64)}}
	removed := pinned[:1]
	helm := cloneVersions(okTools, "helm", "v4.4.0+gdeadbee")
	kubectl := cloneVersions(okTools, "kubectl", "v1.38.0")
	dockerClient := cloneVersions(okTools, "docker-client", "30.0.0")
	dockerServer := cloneVersions(okTools, "docker-server", "30.0.0")
	for name, other := range map[string]map[string]string{
		"kubernetes":    mustManifest(t, withVersion(a, "kubernetes", "v1.38.0"), pinned, okTools),
		"node image":    mustManifest(t, withVersion(a, "node_image", "kindest/node:v1.38.0"), pinned, okTools),
		"kind":          mustManifest(t, withVersion(a, "kind", "v0.34.0"), pinned, okTools),
		"image digest":  mustManifest(t, a, changedDigest, okTools),
		"image removed": mustManifest(t, a, removed, okTools),
		"helm":          mustManifest(t, a, pinned, helm),
		"kubectl":       mustManifest(t, a, pinned, kubectl),
		"docker client": mustManifest(t, a, pinned, dockerClient),
		"docker server": mustManifest(t, a, pinned, dockerServer),
	} {
		if author.EnvironmentHash(other) == hash {
			t.Errorf("a changed %s did not change the environment hash", name)
		}
	}
	// The input maps are not modified.
	testutil.Equal(t, len(a.Versions), 3)
	testutil.Equal(t, len(okTools), 4)
}

func TestEnvironmentManifestRefusesAmbiguousOrPartialInput(t *testing.T) {
	id := model.Identity{ID: "0123456789ab", Versions: map[string]string{"kubernetes": "v1.37.0"}}
	if _, err := author.EnvironmentManifest(id, []environment.PinnedImage{pinned[0], {Name: pinned[0].Name, Digest: pinned[1].Digest}}, okTools); err == nil {
		t.Error("accepted one image name with two digests")
	}
	if _, err := author.EnvironmentManifest(id, []environment.PinnedImage{{Name: "docker.io/library/x:1", Digest: "latest"}}, okTools); err == nil {
		t.Error("accepted an image without a sha256 digest")
	}
	for _, missing := range []string{"helm", "kubectl", "docker-client", "docker-server"} {
		partial := map[string]string{}
		for k, v := range okTools {
			if k != missing {
				partial[k] = v
			}
		}
		if _, err := author.EnvironmentManifest(id, pinned, partial); err == nil {
			t.Errorf("accepted tool versions without %s", missing)
		}
		partial[missing] = " "
		if _, err := author.EnvironmentManifest(id, pinned, partial); err == nil {
			t.Errorf("accepted a blank %s version", missing)
		}
	}
	if _, err := author.EnvironmentManifest(id, pinned, nil); err == nil {
		t.Error("accepted no tool versions at all")
	}
}

// A changed pinned digest or host tool version leaves the problem hash alone
// and still makes the stored certificate stale.
func TestCertificateGoesStaleWithPinsAndTools(t *testing.T) {
	id := model.Identity{ID: "0123456789ab", Versions: map[string]string{"kubernetes": "v1.37.0"}}
	hash := author.EnvironmentHash(mustManifest(t, id, pinned, okTools))
	record := passedRecord("sha256:problem", "grader-1", hash)
	testutil.Equal(t, author.CertificateMatches(record, "sha256:problem", "grader-1", hash), true)

	newDigest := []environment.PinnedImage{{Name: pinned[0].Name, Digest: "sha256:" + strings.Repeat("d", 64)}, pinned[1]}
	testutil.Equal(t, author.CertificateMatches(record, "sha256:problem", "grader-1", author.EnvironmentHash(mustManifest(t, id, newDigest, okTools))), false)
	testutil.Equal(t, author.CertificateMatches(record, "sha256:problem", "grader-1", author.EnvironmentHash(mustManifest(t, id, pinned, cloneVersions(okTools, "helm", "v4.4.0+gdeadbee")))), false)
	testutil.Equal(t, author.CertificateMatches(record, "sha256:problem", "grader-1", author.EnvironmentHash(mustManifest(t, id, []environment.PinnedImage{pinned[1], pinned[0]}, okTools))), true)
}

func withVersion(id model.Identity, key, value string) model.Identity {
	id.Versions = cloneVersions(id.Versions, key, value)
	return id
}

func cloneVersions(in map[string]string, key, value string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}
