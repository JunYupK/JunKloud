//go:build linux

package author

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"sigs.k8s.io/yaml"
)

var authorReleaseName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func renderHelm(ctx context.Context, c ToolActionConfig, ns string, a model.Action, t *toolAction) (grader.ToolResult, error) {
	if err := onlyArgs(a.Args, "operation", "release", "chart", "values"); err != nil {
		return grader.ToolResult{}, err
	}
	if a.Args["operation"] != "install" {
		return grader.ToolResult{}, invalid("only Helm install is supported")
	}
	t.release, _ = a.Args["release"].(string)
	if len(t.release) > 53 || !authorReleaseName.MatchString(t.release) {
		return grader.ToolResult{}, invalid("invalid Helm release name")
	}
	rel, ok := a.Args["chart"].(string)
	if !ok {
		return grader.ToolResult{}, invalid("chart must be a local directory")
	}
	var err error
	t.path, err = scopedPath(t.root, rel, false)
	if err != nil {
		return grader.ToolResult{}, err
	}
	st, err := os.Stat(t.path)
	if err != nil || !st.IsDir() {
		return grader.ToolResult{}, invalid("chart directory does not exist")
	}
	if err = reviewChart(t.path); err != nil {
		return grader.ToolResult{}, err
	}
	values, ok := a.Args["values"].([]any)
	if !ok {
		if vs, yes := a.Args["values"].([]string); yes {
			values = make([]any, len(vs))
			for i, v := range vs {
				values[i] = v
			}
			ok = true
		}
	}
	if !ok || len(values) > 4 {
		return grader.ToolResult{}, invalid("values must contain at most4 local files")
	}
	for _, v := range values {
		p, ok := v.(string)
		if !ok {
			return grader.ToolResult{}, invalid("values path must be a string")
		}
		p, err = scopedPath(t.root, p, false)
		if err != nil {
			return grader.ToolResult{}, err
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return grader.ToolResult{}, invalid("values file does not exist")
		}
		t.values = append(t.values, p)
	}
	args := []string{"template", t.release, t.path}
	for _, v := range t.values {
		args = append(args, "--values", v)
	}
	return toolRun(ctx, c, helmCall(c, t, ns, args...))
}

// The author format is deliberately smaller than ordinary Helm: output can
// depend only on identical values and Release.Name/Namespace. Native install
// cannot change scope through capabilities, lookup, functions or control flow.
func reviewChart(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return invalid("chart needs Chart.yaml")
	}
	var ch map[string]any
	if err = yaml.UnmarshalStrict(b, &ch); err != nil {
		return invalid("invalid Chart.yaml")
	}
	if ch["apiVersion"] != "v2" || ch["name"] == nil || ch["version"] == nil || ch["dependencies"] != nil || ch["type"] != nil && ch["type"] != "application" {
		return invalid("chart must be a local application without dependencies")
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if rel != "templates" {
				return invalid("unsupported chart directory %s", rel)
			}
			return nil
		}
		if rel == "Chart.yaml" || rel == "values.yaml" {
			return nil
		}
		if !strings.HasPrefix(rel, "templates/") || !strings.HasSuffix(rel, ".yaml") && !strings.HasSuffix(rel, ".yml") {
			return invalid("unsupported chart file %s", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return invalid("chart template exceeds limit")
		}
		tree, err := template.New(rel).Funcs(template.FuncMap{"quote": func(any) string { return "" }}).Parse(string(b))
		if err != nil {
			return invalid("unsupported chart template: %v", err)
		}
		if len(tree.Templates()) != 1 || !reviewTemplateNode(tree.Tree.Root) {
			return invalid("chart template exceeds deterministic AST subset")
		}
		return nil
	})
}
func reviewTemplateNode(n parse.Node) bool {
	switch v := n.(type) {
	case *parse.ListNode:
		for _, child := range v.Nodes {
			if !reviewTemplateNode(child) {
				return false
			}
		}
		return true
	case *parse.TextNode, *parse.CommentNode:
		return true
	case *parse.ActionNode:
		if len(v.Pipe.Decl) != 0 || v.Pipe.IsAssign || len(v.Pipe.Cmds) < 1 || len(v.Pipe.Cmds) > 2 {
			return false
		}
		for i, cmd := range v.Pipe.Cmds {
			if len(cmd.Args) != 1 {
				return false
			}
			if i == 1 {
				id, ok := cmd.Args[0].(*parse.IdentifierNode)
				if !ok || id.Ident != "quote" {
					return false
				}
				continue
			}
			switch arg := cmd.Args[0].(type) {
			case *parse.FieldNode:
				if len(arg.Ident) < 2 {
					return false
				}
				if arg.Ident[0] == "Values" {
					continue
				}
				if len(arg.Ident) != 2 || arg.Ident[0] != "Release" || arg.Ident[1] != "Name" && arg.Ident[1] != "Namespace" {
					return false
				}
			case *parse.StringNode, *parse.NumberNode, *parse.BoolNode: // scalar literals
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}
func (e *executor) installHelm(ctx context.Context, o environment.Owner, t *toolAction) error {
	args := []string{"install", t.release, t.path}
	for _, v := range t.values {
		args = append(args, "--values", v)
	}
	result, err := toolRun(ctx, e.tools, helmCall(e.tools, t, o.Namespace, args...))
	if result.ExitCode > 0 && errors.Is(err, environment.ErrInvalidAction) {
		return fmt.Errorf("author: native Helm install failed (exit %d); retain namespace inventory for recovery", result.ExitCode)
	}
	return err
}
