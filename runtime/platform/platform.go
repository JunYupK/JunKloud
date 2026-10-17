// Package platform embeds JunKloud's vendored, version-pinned platform
// files, so nothing is fetched from the internet at run time:
//
//   - metrics-server/components.yaml: the metrics-server release manifest,
//     byte-for-byte as published (sha256 recorded in
//     runtime/versions.lock.yaml, platform.metrics_server.manifest_sha256).
//   - probes/image-build: the build context of the doctor's image-build
//     probe (FROM the pinned probe image).
//
// Image references are pinned by tag and digest in runtime/versions.lock.yaml
// and mirrored by constants in internal/environment (unit-tested).
package platform

import "embed"

// FS holds the vendored platform files at the paths listed above.
//
//go:embed metrics-server/components.yaml probes/image-build/Dockerfile
var FS embed.FS

// Paths inside FS.
const (
	MetricsServerManifest = "metrics-server/components.yaml"
	ImageBuildProbeDir    = "probes/image-build"
)
