//go:build linux

package environment

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestDefaultNodeImageMatchesLock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "runtime", "versions.lock.yaml"))
	testutil.NoError(t, err)
	var lock struct {
		Tools struct {
			Kind struct {
				Version   string `json:"version"`
				NodeImage string `json:"node_image"`
			} `json:"kind"`
		} `json:"tools"`
	}
	testutil.NoError(t, yaml.Unmarshal(raw, &lock))
	testutil.Equal(t, DefaultNodeImage, lock.Tools.Kind.NodeImage)
	testutil.Equal(t, KindVersion, lock.Tools.Kind.Version)
	if !strings.Contains(DefaultNodeImage, "@sha256:") {
		t.Fatalf("node image %q is not pinned by digest", DefaultNodeImage)
	}
}

func TestKindConfigDefaultHasNoHostCompat(t *testing.T) {
	cfg := string(kindConfig("0123456789ab", HostCompatNone))
	for _, s := range []string{"failCgroupV1", "restrict_oom_score_adj"} {
		if strings.Contains(cfg, s) {
			t.Fatalf("default kind config must not contain %s:\n%s", s, cfg)
		}
	}
	var parsed map[string]any
	testutil.NoError(t, yaml.Unmarshal([]byte(cfg), &parsed))
	testutil.Equal(t, parsed["kind"], "Cluster")
	nodes := parsed["nodes"].([]any)
	testutil.Equal(t, len(nodes), 1)
	node := nodes[0].(map[string]any)
	testutil.Equal(t, node["role"], "control-plane")
	testutil.Equal(t, node["labels"], map[string]any{LabelEnvironmentID: "0123456789ab"})
}

func TestKindConfigHostCompatCgroupV1(t *testing.T) {
	cfg := string(kindConfig("0123456789ab", HostCompatCgroupV1))
	var parsed map[string]any
	testutil.NoError(t, yaml.Unmarshal([]byte(cfg), &parsed))
	patches := parsed["containerdConfigPatches"].([]any)
	testutil.Equal(t, len(patches), 1)
	if !strings.Contains(patches[0].(string), "restrict_oom_score_adj = true") {
		t.Fatalf("containerd patch missing: %v", patches[0])
	}
	node := parsed["nodes"].([]any)[0].(map[string]any)
	kubeadm := node["kubeadmConfigPatches"].([]any)
	if !strings.Contains(kubeadm[0].(string), "failCgroupV1: false") {
		t.Fatalf("kubelet patch missing: %v", kubeadm[0])
	}
}

func TestParseHostCompat(t *testing.T) {
	got, err := ParseHostCompat("cgroupv1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, HostCompatCgroupV1)
	got, err = ParseHostCompat("")
	testutil.NoError(t, err)
	testutil.Equal(t, got, HostCompatNone)
	_, err = ParseHostCompat("cgroupv3")
	if err == nil {
		t.Fatal("unknown host compat mode accepted")
	}
}

// TestCreateUsesOwnKubeconfig: kind is always told to write its kubeconfig
// into the state directory, with the pinned image and a config file; it is
// never left to fall back to $KUBECONFIG or ~/.kube/config.
func TestCreateUsesOwnKubeconfig(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{respond: func(string, []string) (executil.Result, error) {
		return executil.Result{}, nil
	}}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	testutil.NoError(t, m.kindCreate(bg, "0123456789ab"))
	calls := fake.Calls()
	testutil.Equal(t, len(calls), 1)
	args := strings.Fields(calls[0])
	for _, want := range [][]string{
		{"--name", "junkloud-0123456789ab"},
		{"--image", DefaultNodeImage},
		{"--kubeconfig", m.KubeconfigPath()},
		{"--config", filepath.Join(m.dir, "kind-config.yaml")},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Fatalf("kind create args %v lack %v", args, want)
		}
	}
}

// TestDefaultRunnerDropsKubeconfig: the default runner gives kind and docker
// an environment without KUBECONFIG, so neither can touch the host's file.
func TestDefaultRunnerDropsKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/somewhere/else")
	dir := t.TempDir()
	m := New(dir, openTestStore(t, dir))
	r, ok := m.runner.(executil.Runner)
	if !ok {
		t.Fatalf("default runner is %T", m.runner)
	}
	for _, kv := range r.Env {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Fatalf("child environment carries %s", kv)
		}
	}
	if len(r.Env) == 0 {
		t.Fatal("child environment is empty")
	}
}

func TestParseContainers(t *testing.T) {
	raw := `[{"Id":"abc","Name":"/junkloud-x-control-plane","State":{"Running":true},
	  "Config":{"Labels":{"io.x-k8s.kind.cluster":"junkloud-x","io.x-k8s.kind.role":"control-plane"}},
	  "NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"},"kind":{"IPAddress":"172.18.0.3"}}}}]`
	nodes, err := parseContainers([]byte(raw))
	testutil.NoError(t, err)
	testutil.Equal(t, nodes, []node{{
		ID: "abc", Name: "junkloud-x-control-plane", Running: true,
		Cluster: "junkloud-x", Role: "control-plane", IP: "172.18.0.3",
	}})
}

func TestParseIDs(t *testing.T) {
	testutil.Equal(t, parseIDs([]byte("b\n\na\n")), []string{"a", "b"})
	testutil.Equal(t, len(parseIDs(nil)), 0)
}

// A tampered identity file cannot smuggle flags into docker.
func TestInspectRejectsNonIDs(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	_, err := m.inspect(bg, []string{"--format={{.}}"})
	testutil.ErrorIs(t, err, ErrIdentityMismatch)
	testutil.Equal(t, len(fake.Calls()), 0)
}
