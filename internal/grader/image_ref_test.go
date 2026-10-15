package grader

import "testing"

func TestNormalizeImageRef(t *testing.T) {
	for in, want := range map[string]string{
		"busybox:1.37.0":                           "docker.io/library/busybox:1.37.0",
		"docker.io/library/busybox:1.37.0":         "docker.io/library/busybox:1.37.0",
		"index.docker.io/library/busybox:1.37.0":   "docker.io/library/busybox:1.37.0",
		"nginx":                                    "docker.io/library/nginx:latest",
		"registry.k8s.io/pause:3.10":               "registry.k8s.io/pause:3.10",
		"localhost:5000/team/app":                  "localhost:5000/team/app:latest",
		"busybox:1.37.0@sha256:" + zeros64:         "docker.io/library/busybox:1.37.0@sha256:" + zeros64,
		"docker.io/someone/tool@sha256:" + zeros64: "docker.io/someone/tool@sha256:" + zeros64,
	} {
		if got := NormalizeImageRef(in); got != want {
			t.Errorf("NormalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

const zeros64 = "0000000000000000000000000000000000000000000000000000000000000000"
