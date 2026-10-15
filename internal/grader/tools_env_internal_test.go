package grader

import (
	"os/exec"
	"strings"
	"testing"
)

// The variables that can redirect or weaken a Kubernetes client. Each must be
// absent from the child environment of helm and kubectl.
var ambientK8sVars = []string{
	"KUBECONFIG", "KUBECTL_EXTERNAL_DIFF", "KUBE_CONFIG_PATH", "KUBERNETES_SERVICE_HOST",
	"HELM_KUBECONFIG", "HELM_KUBECONTEXT", "HELM_KUBEAPISERVER", "HELM_KUBETOKEN", "HELM_KUBEASUSER",
	"HELM_KUBEASGROUPS", "HELM_KUBECAFILE", "HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "HELM_KUBETLS_SERVER_NAME",
	"HELM_DRIVER", "HELM_NAMESPACE", "HELM_REGISTRY_CONFIG", "HELM_PLUGINS", "HELM_DEBUG", "HELM_MAX_HISTORY",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE",
}

func ambient() []string {
	env := []string{"PATH=/usr/bin", "HOME=/home/x", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp", "DOCKER_HOST=unix:///var/run/docker.sock"}
	for _, n := range ambientK8sVars {
		env = append(env, n+"=ambient-value")
	}
	return env
}

func TestKubernetesToolsGetAnAllowlistedEnvironment(t *testing.T) {
	extra := []string{"HELM_CACHE_HOME=/state/cache", "HELM_CONFIG_HOME=/state/config"}
	for _, exe := range []string{"helm", "kubectl", "/usr/local/bin/helm", "/usr/local/bin/kubectl"} {
		got := childEnv(exe, ambient(), extra)
		joined := "\n" + strings.Join(got, "\n") + "\n"
		for _, n := range ambientK8sVars {
			if strings.Contains(joined, "\n"+n+"=") {
				t.Errorf("%s: ambient %s reached the child environment", exe, n)
			}
		}
		for _, want := range []string{"PATH=/usr/bin", "HOME=/home/x", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp", "HELM_CACHE_HOME=/state/cache", "HELM_CONFIG_HOME=/state/config"} {
			if !strings.Contains(joined, "\n"+want+"\n") {
				t.Errorf("%s: %s is missing from %v", exe, want, got)
			}
		}
		if strings.Contains(joined, "DOCKER_HOST") {
			t.Errorf("%s inherits unrelated variables: %v", exe, got)
		}
	}
}

func TestOtherToolsKeepTheEngineButLoseKubernetesVariables(t *testing.T) {
	for _, exe := range []string{"docker", "kind"} {
		got := childEnv(exe, ambient(), nil)
		joined := "\n" + strings.Join(got, "\n") + "\n"
		if !strings.Contains(joined, "\nDOCKER_HOST=unix:///var/run/docker.sock\n") {
			t.Errorf("%s lost DOCKER_HOST: %v", exe, got)
		}
		for _, n := range ambientK8sVars {
			if (strings.HasPrefix(n, "HELM_") || strings.HasPrefix(n, "KUBE")) && strings.Contains(joined, "\n"+n+"=") {
				t.Errorf("%s: ambient %s reached the child environment", exe, n)
			}
		}
	}
}

// A nil Env makes os/exec (and executil.Runner) inherit the whole process
// environment, which would silently undo the isolation whenever nothing is
// allowed through. childEnv must return a non-nil slice, even an empty one.
func TestChildEnvIsNeverNil(t *testing.T) {
	cases := map[string]struct{ base, extra []string }{
		"empty process environment":         {nil, nil},
		"nothing allowlisted":               {[]string{"FOO=bar", "HELM_KUBETOKEN=x", "KUBECONFIG=/k"}, nil},
		"only unrelated variables for helm": {[]string{"HTTPS_PROXY=http://p"}, nil},
	}
	for name, tc := range cases {
		for _, exe := range []string{"helm", "kubectl", "docker", "kind"} {
			got := childEnv(exe, tc.base, tc.extra)
			if got == nil {
				t.Errorf("%s/%s: childEnv returned a nil slice (the child would inherit os.Environ)", name, exe)
			}
		}
		for _, exe := range []string{"helm", "kubectl"} {
			if got := childEnv(exe, tc.base, tc.extra); len(got) != 0 {
				t.Errorf("%s/%s: %v leaked through the allowlist", name, exe, got)
			}
		}
	}
}

// End to end with os/exec: a helm-style child started with the computed
// environment sees nothing of the parent's variables.
func TestNothingIsInheritedWhenNothingIsAllowed(t *testing.T) {
	env, err := exec.LookPath("env")
	if err != nil {
		t.Skip("env is not installed: NOT RUN")
	}
	t.Setenv("HELM_KUBETOKEN", "ambient-token")
	t.Setenv("KUBECONFIG", "/ambient/kubeconfig")
	cmd := exec.Command(env)
	cmd.Env = childEnv("helm", []string{"HELM_KUBETOKEN=ambient-token", "KUBECONFIG=/ambient/kubeconfig"}, nil)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("the child inherited an environment: %q", out)
	}
}
