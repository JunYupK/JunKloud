//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// KindVersion is the kind release JunKloud drives (runtime/versions.lock.yaml).
const KindVersion = "v0.33.0"

// DefaultNodeImage is the kind node image, pinned by digest. It must equal
// tools.kind.node_image in runtime/versions.lock.yaml (unit-tested).
const DefaultNodeImage = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"

// kindNetwork is the Docker network kind attaches nodes to. The API endpoint
// is the control-plane container's address on it, which the JunKloud
// workspace container (on the same network) can reach.
const kindNetwork = "kind"

// Command time limits.
const (
	createTimeout = 10 * time.Minute
	dockerTimeout = time.Minute
)

// HostCompat selects host-compatibility patches for the kind configuration.
// They work around host limitations and are never product defaults: target
// hosts (Docker Desktop, cgroup v2) need none.
type HostCompat string

const (
	// HostCompatNone applies no patches (the default).
	HostCompatNone HostCompat = ""
	// HostCompatCgroupV1 is for Linux hosts still on cgroup v1 that also
	// forbid lowering oom_score_adj (such as the CI sandbox): the kubelet is
	// told not to fail on cgroup v1 (KubeletConfiguration failCgroupV1:
	// false) and containerd keeps oom_score_adj restricted
	// (restrict_oom_score_adj = true).
	HostCompatCgroupV1 HostCompat = "cgroupv1"
)

// ParseHostCompat parses a host-compat mode name ("" or "cgroupv1").
func ParseHostCompat(s string) (HostCompat, error) {
	switch HostCompat(s) {
	case HostCompatNone, HostCompatCgroupV1:
		return HostCompat(s), nil
	}
	return HostCompatNone, fmt.Errorf("environment: unknown host compat mode %q (want \"\" or %q)", s, HostCompatCgroupV1)
}

// kindConfig renders the kind cluster configuration: one control-plane node
// labeled with the environment ID, plus the selected host-compat patches.
func kindConfig(envID string, compat HostCompat) []byte {
	var b strings.Builder
	b.WriteString("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n")
	if compat == HostCompatCgroupV1 {
		b.WriteString("containerdConfigPatches:\n- |-\n  [plugins.\"io.containerd.grpc.v1.cri\"]\n    restrict_oom_score_adj = true\n")
	}
	b.WriteString("nodes:\n- role: control-plane\n  labels:\n")
	fmt.Fprintf(&b, "    %s: %q\n", LabelEnvironmentID, envID)
	if compat == HostCompatCgroupV1 {
		b.WriteString("  kubeadmConfigPatches:\n  - |\n    kind: KubeletConfiguration\n    failCgroupV1: false\n")
	}
	return []byte(b.String())
}

// kindCreate creates the cluster for envID. kind writes its kubeconfig only
// to JunKloud's own path (never $KUBECONFIG or ~/.kube/config).
func (m *Manager) kindCreate(ctx context.Context, envID string) error {
	cfgPath := filepath.Join(m.dir, "kind-config.yaml")
	if err := writeFileAtomic(cfgPath, kindConfig(envID, m.hostCompat)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	_, err := m.runner.Run(ctx, "kind", []string{
		"create", "cluster",
		"--name", ClusterName(envID),
		"--image", m.nodeImage,
		"--config", cfgPath,
		"--kubeconfig", m.KubeconfigPath(),
		"--wait", "5m",
	}, nil)
	if err != nil {
		return fmt.Errorf("environment: create cluster %s: %w", ClusterName(envID), err)
	}
	return nil
}

// kindInternalKubeconfig returns kind's in-network kubeconfig for a cluster
// (printed to stdout; no file is written).
func (m *Manager) kindInternalKubeconfig(ctx context.Context, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	res, err := m.runner.Run(ctx, "kind", []string{"get", "kubeconfig", "--internal", "--name", name}, nil)
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// clusterContainerIDs lists (sorted, full IDs) every container kind labeled
// as a node of the named cluster, running or not.
func (m *Manager) clusterContainerIDs(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	res, err := m.runner.Run(ctx, "docker", []string{
		"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster=" + name,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: list node containers: %v", ErrUnavailable, err)
	}
	return parseIDs(res.Stdout), nil
}

// containerIDPattern matches Docker container IDs (full or abbreviated).
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// node is the part of `docker inspect` JunKloud relies on.
type node struct {
	ID      string
	Name    string
	Running bool
	Cluster string
	Role    string
	IP      string
}

// inspect reads the given containers. A container that no longer exists is
// an identity mismatch: the recorded node is gone.
func (m *Manager) inspect(ctx context.Context, ids []string) ([]node, error) {
	for _, id := range ids {
		// IDs come from the identity file; never let one pass as a flag.
		if !containerIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %q is not a container ID", ErrIdentityMismatch, id)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	res, err := m.runner.Run(ctx, "docker", append([]string{"container", "inspect"}, ids...), nil)
	if err != nil {
		if strings.Contains(string(res.Stderr), "No such container") || strings.Contains(string(res.Stderr), "No such object") {
			return nil, fmt.Errorf("%w: recorded node container missing: %v", ErrIdentityMismatch, err)
		}
		return nil, fmt.Errorf("%w: inspect node containers: %v", ErrUnavailable, err)
	}
	nodes, err := parseContainers(res.Stdout)
	if err != nil {
		return nil, err
	}
	if len(nodes) != len(ids) {
		return nil, fmt.Errorf("%w: inspected %d containers for %d recorded IDs", ErrIdentityMismatch, len(nodes), len(ids))
	}
	return nodes, nil
}

// controlPlane returns the cluster's control-plane node among ids.
func (m *Manager) controlPlane(ctx context.Context, name string, ids []string) (node, error) {
	nodes, err := m.inspect(ctx, ids)
	if err != nil {
		return node{}, err
	}
	for _, n := range nodes {
		if n.Cluster != name {
			return node{}, fmt.Errorf("%w: container %s belongs to cluster %q", ErrIdentityMismatch, n.ID, n.Cluster)
		}
		if n.Role == "control-plane" && n.Running && n.IP != "" {
			return n, nil
		}
	}
	return node{}, fmt.Errorf("%w: cluster %s has no running control-plane on the %q network", ErrRecoveryNeeded, name, kindNetwork)
}

func parseContainers(raw []byte) ([]node, error) {
	var out []struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("environment: decode docker inspect: %w", err)
	}
	nodes := make([]node, 0, len(out))
	for _, c := range out {
		nodes = append(nodes, node{
			ID:      c.ID,
			Name:    strings.TrimPrefix(c.Name, "/"),
			Running: c.State.Running,
			Cluster: c.Config.Labels["io.x-k8s.kind.cluster"],
			Role:    c.Config.Labels["io.x-k8s.kind.role"],
			IP:      c.NetworkSettings.Networks[kindNetwork].IPAddress,
		})
	}
	return nodes, nil
}

// parseIDs splits newline-separated IDs, dropping blanks, sorted.
func parseIDs(raw []byte) []string {
	var ids []string
	for _, f := range strings.Fields(string(raw)) {
		ids = append(ids, f)
	}
	slices.Sort(ids)
	return ids
}
