//go:build linux

package author

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
)

// toolVersionTimeout bounds each version command.
const toolVersionTimeout = 30 * time.Second

// Manifest keys (after the "tool:" prefix) of the host tools whose behaviour
// a verdict depends on: helm and kubectl (kustomize) are run by the artifact
// checkers, docker by the image checks.
var toolKeys = []string{"helm", "kubectl", "docker-client", "docker-server"}

var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$`)

// DetectToolVersions asks the host tools the grader runs for their versions:
// helm (client), kubectl (client) and docker (client and server). It uses the
// given runner, normally grader.NewToolRunner(), so the commands run with the
// same allowlisted environment, no shell and bounded time and output as the
// grading commands. The result has the keys "helm", "kubectl",
// "docker-client" and "docker-server". Any tool that is missing, fails, does
// not answer in time or prints something that is not a version is an error:
// a certificate must never be keyed by a partial or guessed environment.
func DetectToolVersions(ctx context.Context, tools grader.ToolRunner) (map[string]string, error) {
	run := func(exe string, args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, toolVersionTimeout)
		defer cancel()
		res, err := tools.Run(cctx, grader.ToolCall{Exe: exe, Args: args})
		if err != nil {
			return nil, fmt.Errorf("author: detect %s version: %w", exe, err)
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("author: detect %s version: exit code %d: %s", exe, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return res.Stdout, nil
	}

	out, err := run("helm", "version", "--short")
	if err != nil {
		return nil, err
	}
	helm := strings.TrimSpace(string(out))
	if !versionPattern.MatchString(helm) {
		return nil, fmt.Errorf("author: detect helm version: %q is not a version", helm)
	}

	out, err = run("kubectl", "version", "--client", "-o", "json")
	if err != nil {
		return nil, err
	}
	var kc struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		return nil, fmt.Errorf("author: detect kubectl version: %w", err)
	}
	if !versionPattern.MatchString(kc.ClientVersion.GitVersion) {
		return nil, fmt.Errorf("author: detect kubectl version: %q is not a version", kc.ClientVersion.GitVersion)
	}

	out, err = run("docker", "version", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var dv struct {
		Client *struct{ Version string } `json:"Client"`
		Server *struct{ Version string } `json:"Server"`
	}
	if err := json.Unmarshal(out, &dv); err != nil {
		return nil, fmt.Errorf("author: detect docker version: %w", err)
	}
	if dv.Client == nil || dv.Server == nil {
		return nil, errors.New("author: detect docker version: the docker engine did not answer (client and server versions are both required)")
	}
	for _, v := range []string{dv.Client.Version, dv.Server.Version} {
		if !versionPattern.MatchString(v) {
			return nil, fmt.Errorf("author: detect docker version: %q is not a version", v)
		}
	}
	return map[string]string{
		"helm": helm, "kubectl": kc.ClientVersion.GitVersion,
		"docker-client": dv.Client.Version, "docker-server": dv.Server.Version,
	}, nil
}
