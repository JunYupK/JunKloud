package grader

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/JunYupK/JunKloud/internal/problem"
)

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
	metaBytes := 0
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
			(h.Size <= maxArchiveMeta && (strings.HasPrefix(name, "blobs/sha256/") || strings.HasSuffix(name, ".json")))
		if small && h.Typeflag == tar.TypeReg && h.Size <= maxArchiveMeta {
			b, err := io.ReadAll(io.LimitReader(tr, maxArchiveMeta))
			if err != nil {
				return lay, err
			}
			// Only JSON documents are kept (layers are tar/gzip), and only a
			// bounded amount of them, so a huge archive cannot exhaust memory.
			if metaBytes+len(b) > maxArchiveMetaTotal || (len(b) > 0 && b[0] != '{' && b[0] != '[' && name != "oci-layout" && name != "repositories") {
				continue
			}
			metaBytes += len(b)
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
