// Package model holds the plain value types shared between JunKloud
// packages. It has no behaviour beyond constants: validation, rendering,
// grading and persistence live in the packages that own them.
//
// Conventions: unique IDs are strings, times are UTC time.Time, points are
// int, and JSON property names are snake_case.
package model

import "time"

// ProblemSchemaVersion is the only problem.yaml schema version this build
// understands (schemas/problem-v1.schema.json).
const ProblemSchemaVersion = 1

// Primary domain IDs. Every problem has exactly one.
const (
	DomainDesignBuild         = "design-build"
	DomainDeployment          = "deployment"
	DomainObservability       = "observability"
	DomainEnvironmentSecurity = "environment-security"
	DomainNetworking          = "networking"
)

// Problem types.
const (
	TypeBuild        = "build"
	TypeModify       = "modify"
	TypeTroubleshoot = "troubleshoot"
	TypeBehavioral   = "behavioral"
)

// Difficulty levels.
const (
	DifficultyEasy   = "easy"
	DifficultyMedium = "medium"
	DifficultyHard   = "hard"
)

// Criterion kinds. Schema v1 enumerates all of them; each kind's Args are
// validated by the checker that implements it.
const (
	CriterionResource        = "resource"
	CriterionField           = "field"
	CriterionReady           = "ready"
	CriterionHTTP            = "http"
	CriterionTCP             = "tcp"
	CriterionNetworkPolicy   = "network-policy"
	CriterionFile            = "file"
	CriterionImage           = "image"
	CriterionHelm            = "helm"
	CriterionKustomize       = "kustomize"
	CriterionLogs            = "logs"
	CriterionJob             = "job"
	CriterionCronJobTemplate = "cronjob-template"
	CriterionCronJobSchedule = "cronjob-schedule"
	CriterionAuthorization   = "authorization"
	CriterionSecurityContext = "security-context"
)

// Action kinds used by setup, solution and cases. There is deliberately no
// shell or eval action.
const (
	ActionApply       = "apply"
	ActionDeleteOwned = "delete-owned"
	ActionHelm        = "helm"
	ActionImage       = "image"
	ActionFile        = "file"
	ActionPatch       = "patch"
)

// Domains lists every primary domain ID.
func Domains() []string {
	return []string{DomainDesignBuild, DomainDeployment, DomainObservability, DomainEnvironmentSecurity, DomainNetworking}
}

// Types lists every problem type.
func Types() []string {
	return []string{TypeBuild, TypeModify, TypeTroubleshoot, TypeBehavioral}
}

// Difficulties lists every difficulty level.
func Difficulties() []string {
	return []string{DifficultyEasy, DifficultyMedium, DifficultyHard}
}

// CriterionKinds lists every criterion kind known to schema v1.
func CriterionKinds() []string {
	return []string{
		CriterionResource, CriterionField, CriterionReady, CriterionHTTP, CriterionTCP,
		CriterionNetworkPolicy, CriterionFile, CriterionImage, CriterionHelm, CriterionKustomize,
		CriterionLogs, CriterionJob, CriterionCronJobTemplate, CriterionCronJobSchedule,
		CriterionAuthorization, CriterionSecurityContext,
	}
}

// ActionKinds lists every action kind known to schema v1.
func ActionKinds() []string {
	return []string{ActionApply, ActionDeleteOwned, ActionHelm, ActionImage, ActionFile, ActionPatch}
}

// ObjectRef names one Kubernetes object.
type ObjectRef struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// Criterion is one graded check. Timeout zero means the grader default.
type Criterion struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Feedback is the criterion's Korean feedback for the learner (spec
	// 8.1), shown with its result where the mode discloses results. It is
	// rendered with the frozen parameters like the rest of the definition.
	Feedback  string         `json:"feedback"`
	Target    ObjectRef      `json:"target"`
	Points    int            `json:"points"`
	DependsOn []string       `json:"depends_on,omitempty"`
	Timeout   time.Duration  `json:"timeout,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
}

// Action is one declarative setup/solution/case step.
type Action struct {
	Kind string         `json:"kind"`
	Args map[string]any `json:"args,omitempty"`
}

// ResourceBudget is the problem's declared cluster footprint, parsed from
// Kubernetes quantity strings at load time.
type ResourceBudget struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// ResourceScope declares what setup, solution and cases may create beyond
// the problem's own namespace: extra owned namespaces and named
// cluster-scoped objects (Namespace empty). The zero value means only the
// problem namespace and no cluster-scoped objects. Enforcement against applied
// objects belongs to the code that applies them.
type ResourceScope struct {
	Namespaces       []string    `json:"namespaces,omitempty"`
	ClusterResources []ObjectRef `json:"cluster_resources,omitempty"`
}

// Problem is a validated problem.yaml plus the package files it was loaded
// with. Parameters maps each parameter name to its allowed scalar values.
// InitialExpected / Case.Expected map criterion IDs to the expected status.
//
// Files holds every package file except problem.yaml, keyed by slash path
// relative to the package root (for example "solution/answer.yaml"). It is
// private package content and is never serialised with the definition; it is
// covered by the content hash instead.
type Problem struct {
	SchemaVersion   int               `json:"schema_version"`
	ID              string            `json:"id"`
	Version         string            `json:"version"`
	Title           string            `json:"title"`
	Domain          string            `json:"domain"`
	Difficulty      string            `json:"difficulty"`
	Type            string            `json:"type"`
	Tags            []string          `json:"tags,omitempty"`
	Competencies    []string          `json:"competencies"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	Parameters      map[string][]any  `json:"parameters,omitempty"`
	Namespace       string            `json:"namespace"`
	Scope           ResourceScope     `json:"scope"`
	Budget          ResourceBudget    `json:"budget"`
	Sources         []string          `json:"sources,omitempty"`
	Criteria        []Criterion       `json:"criteria"`
	Setup           []Action          `json:"setup,omitempty"`
	Solution        []Action          `json:"solution"`
	InitialExpected map[string]string `json:"initial_expected"`
	InitialScore    int               `json:"initial_score"`
	Cases           []Case            `json:"cases,omitempty"`
	Files           map[string][]byte `json:"-"`
}

// Case is a representative wrong (or alternative correct) answer and the
// result the grader must produce for it.
type Case struct {
	ID            string            `json:"id"`
	Actions       []Action          `json:"actions"`
	Expected      map[string]string `json:"expected"`
	ExpectedScore int               `json:"expected_score"`
}

// FrozenProblem is a problem with its parameters fixed for one session.
// Definition is the rendered definition; Question and Assets are the only
// public content (English question and setup/provided/ files). Hash is the
// content hash of the unrendered package, used as the key under which the
// private package (solution, cases, explanation) is stored. Context is left
// empty by Freeze and filled from the environment identity by the session.
type FrozenProblem struct {
	Definition    Problem           `json:"definition"`
	Hash          string            `json:"hash"`
	Question      string            `json:"question"`
	Parameters    map[string]any    `json:"parameters,omitempty"`
	Assets        map[string][]byte `json:"assets,omitempty"`
	Context       string            `json:"context,omitempty"`
	Namespace     string            `json:"namespace"`
	Seed          uint64            `json:"seed"`
	SeedAlgorithm string            `json:"seed_algorithm"`
}
