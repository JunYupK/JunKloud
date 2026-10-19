package imageexport

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNativeExportFallbackRequiresExactIDAndPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, proof, saveErr string
		want                 bool
	}{
		{"native", "sha256:" + strings.Repeat("1", 64) + " linux/amd64\nsha256:" + strings.Repeat("1", 64) + " linux/amd64\n", `"--platform" requires API version 1.48, but the Docker daemon API version is 1.43`, true},
		{"wrong architecture", "sha256:" + strings.Repeat("1", 64) + " linux/arm64\nsha256:" + strings.Repeat("1", 64) + " linux/arm64\n", `"--platform" requires API version 1.48, but the Docker daemon API version is 1.43`, false},
		{"tag moved", "sha256:" + strings.Repeat("2", 64) + " linux/amd64\nsha256:" + strings.Repeat("1", 64) + " linux/amd64\n", `"--platform" requires API version 1.48, but the Docker daemon API version is 1.43`, false},
		{"disk error", "", "disk full", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := false
			run := func(_ context.Context, args []string) ([]byte, error) {
				if args[0] == "image" {
					return []byte(tc.proof), nil
				}
				if strings.Contains(strings.Join(args, " "), "--platform") {
					return nil, errors.New(tc.saveErr)
				}
				legacy = true
				return nil, nil
			}
			err := Save(context.Background(), run, "local/app:v1", "sha256:"+strings.Repeat("1", 64), "amd64", "/tmp/image.tar")
			if (err == nil) != tc.want || legacy != tc.want {
				t.Fatalf("err=%v legacy=%v", err, legacy)
			}
		})
	}
}
