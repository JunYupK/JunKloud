.PHONY: test test-race test-runtime

# Unit tests. Run inside the Linux dev container (repo root mounted at /src).
test:
	go test ./...

# Unit tests with the race detector. Run inside the dev container, which is
# the only target that carries the Go compiler and the C toolchain -race needs.
test-race:
	go test -race ./...

# Verifies the reproducible Linux dev/workspace environment itself: Linux,
# native-CPU, persistent home volume, and pinned tool versions.
test-runtime:
	bash tests/runtime/workspace_test.sh
