package grader

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// imageArgs are the Args of an "image" criterion (Target stays empty).
//
//	reference:  the learner's image in the Docker engine, "name:tag" (no
//	            digest). Optional when only an archive is graded.
//	exists:     default true
//	config:     {user?, workdir?, entrypoint?, cmd?, env? {K: V} subset,
//	            exposed_ports? ["8080/tcp"], labels? {K: V} subset}
//	archive:    {path, repo_tag?, format?: docker|oci}: a `docker save` /
//	            OCI archive in the session workspace. Its layout (manifest,
//	            config, layers) is verified without extracting anything, and
//	            config is also checked against the archive's image config.
//	run:        {args?, expect_exit_code? (default 0), stdout_contains?,
//	            stdout_matches?, timeout? (default 30s, max 60s),
//	            serve? {port, path? ("/"), status? (200), body_marker}}
//	            actually runs the image as a pod in the cluster (see
//	            ImageRunner). The criterion's own timeout must exceed
//	            run.timeout by 20s (tag, save and load into the node).
//
// Only the current architecture is accepted: an image of another
// architecture is FAIL. The engine's image ID is recorded as the criterion's
// input hash.
type imageArgs struct {
	Reference string        `json:"reference"`
	Exists    *bool         `json:"exists"`
	Config    *imageConfig  `json:"config"`
	Archive   *imageArchive `json:"archive"`
	Run       *imageRun     `json:"run"`

	runTimeout time.Duration
	patterns   []*regexp.Regexp
}

type imageConfig struct {
	User         *string           `json:"user"`
	Workdir      *string           `json:"workdir"`
	Entrypoint   *[]string         `json:"entrypoint"`
	Cmd          *[]string         `json:"cmd"`
	Env          map[string]string `json:"env"`
	ExposedPorts []string          `json:"exposed_ports"`
	Labels       map[string]string `json:"labels"`
}

func (c imageConfig) empty() bool {
	return c.User == nil && c.Workdir == nil && c.Entrypoint == nil && c.Cmd == nil &&
		len(c.Env) == 0 && len(c.ExposedPorts) == 0 && len(c.Labels) == 0
}

type imageArchive struct {
	Path    string `json:"path"`
	RepoTag string `json:"repo_tag"`
	Format  string `json:"format"`
}

type imageRun struct {
	Args           []string    `json:"args"`
	ExpectExitCode *int        `json:"expect_exit_code"`
	StdoutContains []string    `json:"stdout_contains"`
	StdoutMatches  []string    `json:"stdout_matches"`
	Timeout        string      `json:"timeout"`
	Serve          *imageServe `json:"serve"`
}

type imageServe struct {
	Port       int    `json:"port"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	BodyMarker string `json:"body_marker"`
}

const (
	defaultImageRunTimeout = 30 * time.Second
	maxImageRunTimeout     = 60 * time.Second
	// imageRunSetupBudget is what the criterion timeout must add to the run
	// timeout: tag, save and load into the node, pod start and clean-up.
	imageRunSetupBudget = 20 * time.Second
	maxArchiveEntries   = 20000
	maxArchiveMeta      = 4 << 20
)

var imageRefPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,254}$`)

func parseImageArgs(crit model.Criterion, defaultTimeout time.Duration) (imageArgs, error) {
	var a imageArgs
	if err := noTarget(crit); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if a.Reference == "" && a.Archive == nil {
		return a, invalidf("args.reference or args.archive is required")
	}
	if a.Reference != "" && (!imageRefPattern.MatchString(a.Reference) || strings.Contains(a.Reference, "..") || strings.Contains(a.Reference, "//")) {
		return a, invalidf("args.reference %q is not a plain image reference (name:tag)", a.Reference)
	}
	if a.Exists != nil && !*a.Exists && (a.Config != nil || a.Archive != nil || a.Run != nil) {
		return a, invalidf("args.exists: false cannot be combined with config, archive or run")
	}
	if a.Config != nil && a.Reference == "" && a.Archive == nil {
		return a, invalidf("args.config needs a reference or an archive")
	}
	if a.Run != nil && a.Reference == "" {
		return a, invalidf("args.run needs args.reference")
	}
	if a.Config != nil && a.Config.empty() {
		return a, invalidf("args.config is empty")
	}
	if a.Archive != nil {
		clean := path.Clean(a.Archive.Path)
		if a.Archive.Path == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
			return a, invalidf("args.archive.path %q must be a path below the session workspace", a.Archive.Path)
		}
		switch a.Archive.Format {
		case "", "docker", "oci":
		default:
			return a, invalidf("args.archive.format %q is not docker or oci", a.Archive.Format)
		}
		if a.Archive.RepoTag != "" && !imageRefPattern.MatchString(a.Archive.RepoTag) {
			return a, invalidf("args.archive.repo_tag %q is not an image reference", a.Archive.RepoTag)
		}
	}
	if a.Config != nil {
		for _, p := range a.Config.ExposedPorts {
			if _, ok := normalizePort(p); !ok {
				return a, invalidf("args.config.exposed_ports: %q is not a port like 8080 or 8080/tcp", p)
			}
		}
	}
	if r := a.Run; r != nil {
		a.runTimeout = defaultImageRunTimeout
		if r.Timeout != "" {
			d, err := time.ParseDuration(r.Timeout)
			if err != nil || d < time.Second || d > maxImageRunTimeout {
				return a, invalidf("args.run.timeout %q must be a duration of 1s..%s", r.Timeout, maxImageRunTimeout)
			}
			a.runTimeout = d
		}
		effective := crit.Timeout
		if effective <= 0 {
			effective = defaultTimeout
		}
		if effective < a.runTimeout+imageRunSetupBudget {
			return a, invalidf("criterion timeout %s is too short for run.timeout %s (needs at least %s)", effective, a.runTimeout, a.runTimeout+imageRunSetupBudget)
		}
		if err := compileStringsAll(map[string][]string{"stdout_contains": r.StdoutContains}); err != nil {
			return a, err
		}
		var err error
		if a.patterns, err = compilePatterns("run.stdout_matches", r.StdoutMatches); err != nil {
			return a, err
		}
		if r.ExpectExitCode != nil && (*r.ExpectExitCode < 0 || *r.ExpectExitCode > 255) {
			return a, invalidf("args.run.expect_exit_code must be 0..255")
		}
		if s := r.Serve; s != nil {
			if s.Port < 1 || s.Port > 65535 {
				return a, invalidf("args.run.serve.port must be 1..65535")
			}
			if s.Path == "" {
				s.Path = "/"
			}
			if !strings.HasPrefix(s.Path, "/") || strings.ContainsAny(s.Path, " \t\r\n") {
				return a, invalidf("args.run.serve.path must start with /")
			}
			if s.Status == 0 {
				s.Status = 200
			}
			if s.Status < 100 || s.Status > 599 {
				return a, invalidf("args.run.serve.status %d is not an HTTP status", s.Status)
			}
			if s.BodyMarker == "" || len(s.BodyMarker) > maxMarkerLen || strings.ContainsAny(s.BodyMarker, "\r\n") {
				return a, invalidf("args.run.serve.body_marker is required (one line, up to %d characters)", maxMarkerLen)
			}
			if len(r.StdoutContains)+len(r.StdoutMatches) > 0 || r.ExpectExitCode != nil {
				return a, invalidf("args.run.serve cannot be combined with an exit code or stdout rules")
			}
		}
	}
	return a, nil
}

func normalizePort(p string) (string, bool) {
	num, proto, hasProto := strings.Cut(p, "/")
	if !hasProto {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return "", false
	}
	n := 0
	if num == "" || len(num) > 5 {
		return "", false
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", false
		}
		n = n*10 + int(r-'0')
	}
	if n < 1 || n > 65535 {
		return "", false
	}
	return fmt.Sprintf("%d/%s", n, proto), true
}

// ImageRunRequest asks for a learner's image to be run in the cluster.
type ImageRunRequest struct {
	Identity  model.Identity
	ProblemID string
	// LocalRef is the image's reference in the Docker engine.
	LocalRef string
	// Args replace the image's CMD when non-nil.
	Args    []string
	Timeout time.Duration
	// Serve, when set, keeps the container running and requests it over
	// HTTP from a probe pod instead of waiting for it to exit.
	Serve *ImageServe
}

// ImageServe is the HTTP check of a serving image.
type ImageServe struct {
	Port       int
	Path       string
	Status     int
	BodyMarker string
}

// ImageRunResult is how the run ended. A run that could not be carried out
// for reasons of the environment is an error from Run instead.
type ImageRunResult struct {
	// StartError: the container could not be started from the image (for
	// example its entrypoint does not exist). The learner's image.
	StartError string
	// Completed: the container exited with ExitCode (run-to-completion).
	Completed bool
	ExitCode  int
	// TimedOut: the container was still running at the timeout.
	TimedOut bool
	// Output is the container's log (bounded). Never stored in evidence.
	Output string
	// Probe is the HTTP probe result when Serve was requested and the
	// container was still running.
	Probe ProbeResult
}

// ImageRunner runs a learner's image as a pod in the environment's cluster
// (see NewClusterImageRunner). An error means the run could not be judged:
// graded ERROR.
type ImageRunner interface {
	Run(ctx context.Context, req ImageRunRequest) (ImageRunResult, error)
}

// ----------------------------------------------------------------- check

type dockerImage struct {
	ID           string   `json:"Id"`
	RepoTags     []string `json:"RepoTags"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	Config       struct {
		User         string              `json:"User"`
		WorkingDir   string              `json:"WorkingDir"`
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"Config"`
}

// imageView is the part of an image config the criterion can compare; it is
// filled from the engine (docker inspect) or from an archive's config blob.
type imageView struct {
	user, workdir        string
	entrypoint, cmd      []string
	env                  map[string]string
	ports                map[string]bool
	labels               map[string]string
	architecture, osName string
}

func viewOf(d dockerImage) imageView {
	v := imageView{user: d.Config.User, workdir: d.Config.WorkingDir, entrypoint: d.Config.Entrypoint, cmd: d.Config.Cmd,
		labels: d.Config.Labels, architecture: d.Architecture, osName: d.Os, env: map[string]string{}, ports: map[string]bool{}}
	for _, e := range d.Config.Env {
		k, val, _ := strings.Cut(e, "=")
		v.env[k] = val
	}
	for p := range d.Config.ExposedPorts {
		v.ports[p] = true
	}
	return v
}

func (c *ArtifactChecker) hostArch() string { return runtime.GOARCH }

func (cfg imageConfig) failures(v imageView, label string) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, label+": "+fmt.Sprintf(f, a...)) }
	if cfg.User != nil && strings.TrimSpace(v.user) != *cfg.User {
		add("user is %q, want %q", v.user, *cfg.User)
	}
	if cfg.Workdir != nil && v.workdir != *cfg.Workdir {
		add("workdir is %q, want %q", v.workdir, *cfg.Workdir)
	}
	if cfg.Entrypoint != nil && !equalStrings(v.entrypoint, *cfg.Entrypoint) {
		add("entrypoint is %v, want %v", v.entrypoint, *cfg.Entrypoint)
	}
	if cfg.Cmd != nil && !equalStrings(v.cmd, *cfg.Cmd) {
		add("cmd is %v, want %v", v.cmd, *cfg.Cmd)
	}
	for _, k := range sortedKeys(cfg.Env) {
		got, ok := v.env[k]
		switch {
		case !ok:
			add("environment variable %s is not set", k)
		case got != cfg.Env[k]:
			add("environment variable %s has another value", k) // values are not echoed
		}
	}
	for _, p := range cfg.ExposedPorts {
		n, _ := normalizePort(p)
		if !v.ports[n] {
			add("port %s is not exposed", n)
		}
	}
	for _, k := range sortedKeys(cfg.Labels) {
		if got, ok := v.labels[k]; !ok || got != cfg.Labels[k] {
			add("label %s is missing or differs", k)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *ArtifactChecker) checkImage(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseImageArgs(crit, c.set.defaultTimeout)
	if err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	expected := map[string]any{"exists": a.Exists == nil || *a.Exists}
	if a.Reference != "" {
		expected["reference"] = a.Reference
	}
	actual := map[string]any{}
	var failures []string
	imageID := ""

	if a.Reference != "" {
		if c.cfg.Tools == nil {
			return model.CheckResult{}, notConfigured("image checks need the docker CLI (ArtifactConfig.Tools)")
		}
		img, found, err := c.inspectImage(ctx, a.Reference)
		if err != nil {
			return model.CheckResult{}, err
		}
		actual["exists"] = found
		switch {
		case a.Exists != nil && !*a.Exists:
			if found {
				failures = append(failures, fmt.Sprintf("image %s must not exist but it does", a.Reference))
			}
			return c.verdictResult(start, len(failures) == 0, strings.Join(failures, "; "), expected, actual), nil
		case !found:
			return c.verdictResult(start, false, fmt.Sprintf("image %s does not exist in the Docker engine", a.Reference), expected, actual), nil
		}
		imageID = img.ID
		actual["image_id"], actual["architecture"], actual["os"] = img.ID, img.Architecture, img.Os
		if len(img.RepoDigests) > 0 {
			actual["repo_digests"] = img.RepoDigests
		}
		if img.Os != "linux" || img.Architecture != c.hostArch() {
			failures = append(failures, fmt.Sprintf("image is built for %s/%s but this environment runs linux/%s", img.Os, img.Architecture, c.hostArch()))
		}
		if a.Config != nil {
			expected["config"] = a.Config
			failures = append(failures, a.Config.failures(viewOf(img), "image config")...)
		}
	}

	if a.Archive != nil {
		af, aerr := c.checkArchive(t, a)
		if aerr != nil {
			return model.CheckResult{}, aerr
		}
		expected["archive"] = a.Archive.Path
		actual["archive"] = af.info
		failures = append(failures, af.failures...)
	}

	if a.Run != nil && len(failures) == 0 {
		runFailures, runActual, err := c.runImage(ctx, t, a)
		if err != nil {
			return model.CheckResult{}, err
		}
		expected["run"] = true
		actual["run"] = runActual
		failures = append(failures, runFailures...)
	}

	res := c.verdictResult(start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	res.Evidence.InputHash = imageID
	return res, nil
}

// inspectImage asks the engine about one reference. found=false only when the
// engine says the image does not exist.
func (c *ArtifactChecker) inspectImage(ctx context.Context, ref string) (dockerImage, bool, error) {
	res, err := c.cfg.Tools.Run(ctx, ToolCall{Exe: c.cfg.DockerBin, Args: []string{"image", "inspect", ref}})
	if err != nil {
		return dockerImage{}, false, fmt.Errorf("docker image inspect: %w", err)
	}
	if res.ExitCode != 0 {
		stderr := strings.ToLower(string(res.Stderr))
		if strings.Contains(stderr, "no such image") && !strings.Contains(stderr, "cannot connect") {
			return dockerImage{}, false, nil
		}
		return dockerImage{}, false, fmt.Errorf("docker image inspect %s failed (exit %d): %s", ref, res.ExitCode, shortenLine(string(res.Stderr)))
	}
	var list []dockerImage
	if err := json.Unmarshal(res.Stdout, &list); err != nil || len(list) != 1 {
		return dockerImage{}, false, fmt.Errorf("docker image inspect %s: unreadable answer", ref)
	}
	return list[0], true, nil
}

// runImage runs the image in the cluster and judges the outcome.
func (c *ArtifactChecker) runImage(ctx context.Context, t Target, a imageArgs) ([]string, map[string]any, error) {
	if c.cfg.Images == nil {
		return nil, nil, notConfigured("image run checks need an ImageRunner")
	}
	req := ImageRunRequest{Identity: t.Identity, ProblemID: t.Problem.Definition.ID, LocalRef: a.Reference,
		Args: a.Run.Args, Timeout: a.runTimeout}
	if s := a.Run.Serve; s != nil {
		req.Serve = &ImageServe{Port: s.Port, Path: s.Path, Status: s.Status, BodyMarker: s.BodyMarker}
	}
	res, err := c.cfg.Images.Run(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("run image %s: %w", a.Reference, err)
	}
	actual := map[string]any{}
	if res.StartError != "" {
		actual["started"] = false
		return []string{"the container cannot start from the image: " + shortenLine(res.StartError)}, actual, nil
	}
	var failures []string
	if s := a.Run.Serve; s != nil {
		if res.Completed {
			actual["exit_code"] = res.ExitCode
			return []string{fmt.Sprintf("the container exited with code %d instead of serving on port %d", res.ExitCode, s.Port)}, actual, nil
		}
		p := res.Probe
		actual["http_status"], actual["failure_class"], actual["marker"] = p.HTTPStatus, p.FailureClass, p.BodyMarker != ""
		switch {
		case p.FailureClass == FailureProbeError || p.FailureClass == FailureNetwork:
			return nil, nil, fmt.Errorf("%w: the HTTP request to the running image could not be judged (%s)", ErrInsufficientEvidence, p.FailureClass)
		case !p.Connected:
			failures = append(failures, fmt.Sprintf("nothing answered on port %d (%s)", s.Port, orDefault(p.FailureClass, "no answer")))
		case p.HTTPStatus != s.Status:
			failures = append(failures, fmt.Sprintf("GET %s answered %d, want %d", s.Path, p.HTTPStatus, s.Status))
		case p.BodyMarker == "":
			failures = append(failures, fmt.Sprintf("GET %s did not return the expected application marker", s.Path))
		}
		return failures, actual, nil
	}
	if res.TimedOut {
		actual["timed_out"] = true
		return []string{fmt.Sprintf("the container was still running after %s; it must run to completion", a.runTimeout)}, actual, nil
	}
	actual["exit_code"] = res.ExitCode
	want := 0
	if a.Run.ExpectExitCode != nil {
		want = *a.Run.ExpectExitCode
	}
	if res.ExitCode != want {
		failures = append(failures, fmt.Sprintf("the container exited with code %d, want %d", res.ExitCode, want))
	}
	failures = append(failures, prefixAll("output ", textRuleFailures(res.Output, a.Run.StdoutContains, nil, a.patterns))...)
	return failures, actual, nil
}

func prefixAll(prefix string, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = prefix + s
	}
	return out
}

// ---------------------------------------------------------------- archive

type archiveFinding struct {
	failures []string
	info     map[string]any
}

// checkArchive verifies a saved image archive in the workspace.
func (c *ArtifactChecker) checkArchive(t Target, a imageArgs) (archiveFinding, error) {
	out := archiveFinding{info: map[string]any{}}
	if t.Workspace == "" {
		return out, errors.New("no session workspace is configured for this target")
	}
	fail := func(f string, v ...any) (archiveFinding, error) {
		out.failures = append(out.failures, fmt.Sprintf(f, v...))
		return out, nil
	}
	real, err := problem.ResolveAsset(t.Workspace, a.Archive.Path)
	switch {
	case errors.Is(err, problem.ErrAssetEscape):
		return fail("archive %s resolves to a location outside the session workspace; it is not read", a.Archive.Path)
	case err != nil && errors.Is(err, os.ErrNotExist):
		return fail("archive %s does not exist", a.Archive.Path)
	case err != nil:
		return out, fmt.Errorf("resolve archive: %w", err)
	}
	st, err := os.Stat(real)
	if errors.Is(err, os.ErrNotExist) {
		return fail("archive %s does not exist", a.Archive.Path)
	}
	if err != nil {
		return out, fmt.Errorf("stat archive: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fail("archive %s is not a regular file", a.Archive.Path)
	}
	f, err := os.Open(real)
	if err != nil {
		return out, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	lay, err := readArchive(f)
	if err != nil {
		return fail("archive %s is not a readable image tar: %v", a.Archive.Path, err)
	}
	out.info["entries"], out.info["layout"] = lay.entries, lay.layout
	if lay.problem != "" {
		return fail("archive %s: %s", a.Archive.Path, lay.problem)
	}
	if want := a.Archive.Format; (want == "docker" && !lay.docker) || (want == "oci" && !lay.oci) {
		return fail("archive layout is %s, want %s", lay.layout, want)
	}
	tag := a.Archive.RepoTag
	if tag == "" {
		tag = a.Reference
	}
	if tag != "" {
		out.info["repo_tags"] = lay.tags
		if !tagListed(lay.tags, tag) {
			return fail("archive does not contain the image tag %s (it has %v)", tag, lay.tags)
		}
	}
	out.info["layers"] = lay.layers
	if a.Config != nil {
		out.failures = append(out.failures, a.Config.failures(lay.view, "archive image config")...)
	}
	if lay.view.architecture != c.hostArch() || lay.view.osName != "linux" {
		out.failures = append(out.failures, fmt.Sprintf("archive image is built for %s/%s but this environment runs linux/%s", lay.view.osName, lay.view.architecture, c.hostArch()))
	}
	return out, nil
}

func tagListed(tags []string, want string) bool {
	w := normalizeImage(want)
	for _, t := range tags {
		if t == want || normalizeImage(t) == w {
			return true
		}
	}
	return false
}

type archiveLayout struct {
	layout  string // "docker", "oci" or "docker+oci" (what the tar carries)
	docker  bool
	oci     bool
	entries int
	tags    []string
	layers  int
	view    imageView
	problem string // a structural defect (the archive is not a complete image)
}

// readArchive inspects a docker-save or OCI image tar in one pass without
// extracting it: only the small metadata files are kept in memory.
func readArchive(r io.Reader) (archiveLayout, error) {
	var lay archiveLayout
	names := map[string]bool{}
	meta := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return lay, err
		}
		lay.entries++
		if lay.entries > maxArchiveEntries {
			return lay, errors.New("too many entries")
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "./")
		names[name] = true
		small := name == "manifest.json" || name == "index.json" || name == "oci-layout" || name == "repositories" ||
			(h.Size <= maxArchiveMeta && h.Typeflag == tar.TypeReg && (strings.HasPrefix(name, "blobs/sha256/") || strings.HasSuffix(name, ".json")))
		if small && h.Typeflag == tar.TypeReg && h.Size <= maxArchiveMeta {
			b, err := io.ReadAll(io.LimitReader(tr, maxArchiveMeta))
			if err != nil {
				return lay, err
			}
			meta[name] = b
		}
	}
	lay.docker = meta["manifest.json"] != nil
	lay.oci = meta["index.json"] != nil && names["oci-layout"]
	switch {
	case lay.docker && lay.oci:
		lay.layout = "docker+oci"
	case lay.docker:
		lay.layout = "docker"
	case lay.oci:
		lay.layout = "oci"
	}
	switch {
	case lay.docker:
		return readDockerManifest(lay, names, meta)
	case meta["index.json"] != nil:
		return readOCIIndex(lay, names, meta)
	}
	return lay, errors.New("neither manifest.json nor index.json is present")
}

func readDockerManifest(lay archiveLayout, names map[string]bool, meta map[string][]byte) (archiveLayout, error) {
	var manifest []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(meta["manifest.json"], &manifest); err != nil || len(manifest) == 0 {
		return lay, errors.New("manifest.json is not a list of images")
	}
	m := manifest[0]
	lay.tags, lay.layers = m.RepoTags, len(m.Layers)
	cfg, ok := meta[path.Clean(m.Config)]
	if !ok {
		lay.problem = fmt.Sprintf("the image config %s named by manifest.json is missing", m.Config)
		return lay, nil
	}
	for _, l := range m.Layers {
		if !names[path.Clean(l)] {
			lay.problem = fmt.Sprintf("layer %s named by manifest.json is missing", l)
			return lay, nil
		}
	}
	view, err := viewOfConfigBlob(cfg)
	if err != nil {
		lay.problem = "the image config is not valid JSON: " + err.Error()
		return lay, nil
	}
	lay.view = view
	return lay, nil
}

func readOCIIndex(lay archiveLayout, names map[string]bool, meta map[string][]byte) (archiveLayout, error) {
	if lay.layout == "" {
		lay.layout = "oci"
		lay.oci = true
	}
	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
		Platform    *struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	}
	type document struct {
		Manifests []descriptor `json:"manifests"`
		Config    descriptor   `json:"config"`
		Layers    []descriptor `json:"layers"`
	}
	blob := func(d string) ([]byte, string) {
		algo, hexd, ok := strings.Cut(d, ":")
		if !ok {
			return nil, ""
		}
		p := "blobs/" + algo + "/" + hexd
		return meta[p], p
	}
	var idx document
	if err := json.Unmarshal(meta["index.json"], &idx); err != nil || len(idx.Manifests) == 0 {
		return lay, errors.New("index.json lists no manifests")
	}
	for _, d := range idx.Manifests {
		for _, k := range []string{"io.containerd.image.name", "org.opencontainers.image.ref.name"} {
			if n := d.Annotations[k]; n != "" {
				lay.tags = append(lay.tags, n)
			}
		}
	}
	sort.Strings(lay.tags)
	// Descend to a single-platform manifest (an index may nest once).
	cur := idx.Manifests[0]
	for depth := 0; depth < 3; depth++ {
		raw, p := blob(cur.Digest)
		if raw == nil {
			lay.problem = fmt.Sprintf("the manifest blob %s named by the index is missing", cur.Digest)
			return lay, nil
		}
		_ = p
		var doc document
		if err := json.Unmarshal(raw, &doc); err != nil {
			lay.problem = "a manifest blob is not valid JSON"
			return lay, nil
		}
		if len(doc.Manifests) > 0 {
			next := doc.Manifests[0]
			for _, m := range doc.Manifests {
				if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
					next = m
					break
				}
			}
			cur = next
			continue
		}
		cfg, cp := blob(doc.Config.Digest)
		if cfg == nil {
			lay.problem = fmt.Sprintf("the image config blob %s is missing", doc.Config.Digest)
			return lay, nil
		}
		_ = cp
		lay.layers = len(doc.Layers)
		for _, l := range doc.Layers {
			algo, hexd, _ := strings.Cut(l.Digest, ":")
			if !names["blobs/"+algo+"/"+hexd] {
				lay.problem = fmt.Sprintf("layer blob %s is missing", l.Digest)
				return lay, nil
			}
		}
		view, err := viewOfConfigBlob(cfg)
		if err != nil {
			lay.problem = "the image config is not valid JSON: " + err.Error()
			return lay, nil
		}
		lay.view = view
		return lay, nil
	}
	lay.problem = "the manifest index nests too deeply"
	return lay, nil
}

// viewOfConfigBlob reads an image config JSON (docker or OCI form).
func viewOfConfigBlob(raw []byte) (imageView, error) {
	var cfg struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct {
			User         string              `json:"User"`
			WorkingDir   string              `json:"WorkingDir"`
			Entrypoint   []string            `json:"Entrypoint"`
			Cmd          []string            `json:"Cmd"`
			Env          []string            `json:"Env"`
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
			Labels       map[string]string   `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return imageView{}, err
	}
	var d dockerImage
	d.Architecture, d.Os = cfg.Architecture, cfg.OS
	d.Config.User, d.Config.WorkingDir, d.Config.Entrypoint, d.Config.Cmd = cfg.Config.User, cfg.Config.WorkingDir, cfg.Config.Entrypoint, cfg.Config.Cmd
	d.Config.Env, d.Config.ExposedPorts, d.Config.Labels = cfg.Config.Env, cfg.Config.ExposedPorts, cfg.Config.Labels
	return viewOf(d), nil
}
