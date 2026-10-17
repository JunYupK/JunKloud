package model

import "time"

// Identity pins one JunKloud-owned cluster environment.
type Identity struct {
	ID           string            `json:"id"`
	ClusterName  string            `json:"cluster_name"`
	Context      string            `json:"context"`
	Endpoint     string            `json:"endpoint"`
	CAHash       string            `json:"ca_hash"`
	MarkerUID    string            `json:"marker_uid"`
	ContainerIDs []string          `json:"container_ids,omitempty"`
	Versions     map[string]string `json:"versions,omitempty"`
}

// OwnedResource is one Kubernetes object JunKloud created and owns.
type OwnedResource struct {
	Ref     ObjectRef `json:"ref"`
	UID     string    `json:"uid"`
	OwnerID string    `json:"owner_id"`
}

// Inventory lists everything JunKloud created for one session problem.
type Inventory struct {
	EnvironmentID string          `json:"environment_id"`
	SessionID     string          `json:"session_id,omitempty"`
	ProblemID     string          `json:"problem_id,omitempty"`
	Resources     []OwnedResource `json:"resources,omitempty"`
	DockerIDs     []string        `json:"docker_ids,omitempty"`
	ArtifactPaths []string        `json:"artifact_paths,omitempty"`
}

// Operation is an intent recorded before any real resource is created, so an
// interrupted operation can be recovered.
type Operation struct {
	ID            string    `json:"id"`
	EnvironmentID string    `json:"environment_id"`
	SessionID     string    `json:"session_id,omitempty"`
	Kind          string    `json:"kind"`
	Phase         string    `json:"phase"`
	Planned       Inventory `json:"planned"`
	CreatedAt     time.Time `json:"created_at"`
}
