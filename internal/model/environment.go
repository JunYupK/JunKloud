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
	EnvironmentID string       `json:"environment_id"`
	SessionID     string       `json:"session_id,omitempty"`
	ProblemID     string       `json:"problem_id,omitempty"`
	Images        []OwnedImage `json:"images,omitempty"`
	// ObservedImages are declared artifacts, never evidence of ownership.
	ObservedImages []OwnedImage    `json:"observed_images,omitempty"`
	Volumes        []VolumeCleanup `json:"volumes,omitempty"`
	Resources      []OwnedResource `json:"resources,omitempty"`
	DockerIDs      []string        `json:"docker_ids,omitempty"`
	ArtifactPaths  []string        `json:"artifact_paths,omitempty"`
	// Attempts pins grading intent to each problem's submitted attempt.
	// Existing journals without this optional field belong to attempt 1.
	Attempts map[string]int `json:"attempts,omitempty"`
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

// VolumeCleanup records the transitive PVC -> PV -> local-path data identity
// before deleting the namespace. Unsupported storage remains a visible error.
type VolumeCleanup struct {
	Name           string `json:"name"`
	UID            string `json:"uid"`
	ClaimNamespace string `json:"claim_namespace"`
	ClaimName      string `json:"claim_name"`
	ClaimUID       string `json:"claim_uid"`
	ReclaimPolicy  string `json:"reclaim_policy"`
	Provisioner    string `json:"provisioner"`
	Path           string `json:"path"`
	NodeName       string `json:"node_name"`
}

// OwnedImage is an exact build artifact. Cleanup requires its immutable ID,
// reference and all three ownership labels; shared/moved tags refuse cleanup.
type OwnedImage struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
}
