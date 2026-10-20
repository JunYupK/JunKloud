//go:build linux

package environment

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Catch a doctor that bypasses the verified native export path even though
// ordinary learner image saves already use it. Only the Docker/Kubernetes
// boundaries are fake; the real probe builds, exports, loads and checks its Pod.
func TestDoctorImageBuildVerifiedExport(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("a", 64)
	otherID := "sha256:" + strings.Repeat("b", 64)
	unsupported := errors.New(`"--platform" requires API version 1.48, but the Docker daemon API version is 1.43`)
	for _, tc := range []struct {
		name, buildOutput, tagID, arch string
		saveErr                        error
		wantOK                         bool
		legacySaves                    int
	}{
		{"modern native", imageID + "\n", imageID, "amd64", nil, true, 0},
		{"legacy native", imageID + "\n", imageID, "amd64", unsupported, true, 1},
		{"quiet build ID required", "not-an-image-id\n", imageID, "amd64", nil, false, 0},
		{"moved tag", imageID + "\n", otherID, "amd64", nil, false, 0},
		{"wrong architecture", imageID + "\n", imageID, "arm64", nil, false, 0},
		{"unrelated export failure", imageID + "\n", imageID, "amd64", errors.New("disk full"), false, 0},
		{"other API error", imageID + "\n", imageID, "amd64", errors.New("API version 1.43 request failed"), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
				if exe == "docker" && args[0] == "build" {
					return executil.Result{Stdout: []byte(tc.buildOutput)}, nil
				}
				if exe == "docker" && args[0] == "image" && args[1] == "inspect" {
					if args[len(args)-1] != imageID {
						t.Fatalf("export proof did not pin quiet build ID: %v", args)
					}
					return executil.Result{Stdout: []byte(tc.tagID + " linux/" + tc.arch + "\n" + imageID + " linux/amd64\n")}, nil
				}
				if exe == "docker" && args[0] == "save" && args[1] == "--platform" {
					return executil.Result{}, tc.saveErr
				}
				return executil.Result{}, nil
			}}
			client := kubefake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64"}}})
			client.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				pod := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
				pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "probe-token"}}}}}
				return false, nil, nil
			})
			dir := t.TempDir()
			m := New(dir, openTestStore(t, dir), WithRunner(runner))
			p := &prober{m: m, cl: &clients{kube: client}, id: model.Identity{ID: "0123456789ab", ClusterName: "owned"}, run: "test", token: "probe-token"}
			err := p.checkImageBuild(bg)
			if (err == nil) != tc.wantOK {
				t.Fatalf("probe success=%t, want %t: %v", err == nil, tc.wantOK, err)
			}
			testutil.Equal(t, runner.count("docker save --output "), tc.legacySaves)
			pods, err := client.CoreV1().Pods(PlatformNamespace).List(bg, metav1.ListOptions{})
			testutil.NoError(t, err)
			if tc.wantOK {
				testutil.Equal(t, runner.count("kind load image-archive "), 1)
				testutil.Equal(t, len(pods.Items), 1)
				testutil.Equal(t, pods.Items[0].Spec.Containers[0].Image, ImageBuildProbeRepository+":0123456789ab-test")
			} else {
				testutil.Equal(t, runner.count("kind load image-archive "), 0)
				testutil.Equal(t, len(pods.Items), 0)
			}
		})
	}
}
