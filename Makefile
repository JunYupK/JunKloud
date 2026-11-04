.PHONY: test test-race test-runtime test-integration test-e2e test-acceptance lint

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

# Integration tests against real kind clusters they create and delete
# themselves (needs Docker and kind). On a cgroup v1 host that forbids
# lowering oom_score_adj, set JUNKLOUD_TEST_HOST_COMPAT=cgroupv1; on an
# engine kernel with IPv6 disabled add no-ipv6 (cgroupv1,no-ipv6). A host
# known to lack a capability lists it in JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES
# (the doctor must then report it FAIL).
test-integration:
	go test -tags=integration ./tests/integration/... -count=1

# End-to-end tests of the ckad CLI.
test-e2e:
	go test -tags=e2e ./tests/e2e/... -count=1 -timeout=60m

# Release acceptance: the full mock lifecycle (prepare, model solution, grade,
# reset) three times in a row on a real kind cluster. Takes hours. On a host
# that lacks a capability list it in JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES;
# the run is then labelled PARTIAL and never reported as mock-01. Set
# JUNKLOUD_ACCEPTANCE_EVIDENCE to keep the per-cycle JSON evidence.
test-acceptance:
	go test -tags=acceptance ./tests/e2e/... -count=1 -v -timeout=6h

# Static checks for every build tag, and gofmt.
lint:
	go vet ./... && go vet -tags=integration ./... && go vet -tags=e2e ./tests/e2e/ && go vet -tags=acceptance ./tests/e2e/ && test -z "$$(gofmt -l .)"
