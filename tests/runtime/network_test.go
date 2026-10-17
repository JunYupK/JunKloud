//go:build linux

package runtime_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKindNetworkBootstrapOnlyCreatesMissingBridge(t *testing.T) {
	for _, tc := range []struct {
		name, driver  string
		missing, fail bool
	}{{"existing", "bridge", false, false}, {"missing", "bridge", true, false}, {"wrong-driver", "overlay", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			fake := `#!/bin/sh
echo "$*" >> "$CALLS"
if [ "$1 $2" = "network inspect" ]; then
 if [ "$MISSING" = yes ]; then exit 1; fi
 echo "$DRIVER"; exit 0
fi
if [ "$1 $2" = "network create" ]; then exit 0; fi
exit 9
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "../../runtime/ensure-kind-network.sh")
			missing := "no"
			if tc.missing {
				missing = "yes"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+log, "DRIVER="+tc.driver, "MISSING="+missing)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatal(string(out), err)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			created := strings.Contains(string(calls), "network create")
			if created != tc.missing {
				t.Fatal(string(calls))
			}
			if strings.Contains(string(calls), "delete") || strings.Contains(string(calls), "prune") {
				t.Fatal(string(calls))
			}
		})
	}
}

func TestWorkspaceEntrypointSelectsDedicatedKubeconfig(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{".bashrc", ".bash_aliases"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("keep\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(home, "state")
	cmd := exec.Command("bash", "../../runtime/entrypoint.sh", "printenv", "KUBECONFIG")
	cmd.Env = append(os.Environ(), "HOME="+home, "CKAD_STATE_DIR="+state, "KUBECONFIG=/ambient/host-config")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	if strings.TrimSpace(string(out)) != filepath.Join(state, "environment", "kubeconfig") {
		t.Fatal("workspace used ambient kubeconfig", string(out))
	}
	out, err = os.ReadFile(filepath.Join(home, ".bashrc"))
	if err != nil || string(out) != "keep\n" {
		t.Fatal("dotfile changed", string(out), err)
	}
}
