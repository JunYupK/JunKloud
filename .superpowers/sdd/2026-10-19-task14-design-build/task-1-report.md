# Task14 milestone1 report

Status: implementation ready; LIVE PENDING. Completion is not claimed until
the SHA-specific focused workflow finishes and the controller reviews it.

Scope: file/image solution and case execution; managed learner image build,
owned cleanup/recovery and declared-reference observation; native image export.
No Task2 volume checker, Task3 product content, Helm/Kustomize/patch framework,
push or merge. Existing isolated checkout and branch were reused.

Contracts and files:

- `internal/author/{executor,file_actions,image_actions}.go`: full-list
  argument/path/context preflight, private file sources and image build/save,
  shared staged workspace; apply/delete-owned retain Manager.Apply's list
  preflight. Runtime mixed lists additionally use Manager.PreflightActions.
- `author/validate.go` stages assets first, passes the same path to actions
  and grader, rejects file/image setup before preparing the environment.
  `compose.go` provides the real shared Manager builder and pinned images.
- `environment/{image_build,image_context,image_save,preflight_actions}.go`:
  bounded offline native build, intent-before-effects, exact image/tag/labels,
  refusal to adopt foreign/shared tags, same-owner prior-ID cleanup, recovery
  with unique operation proof, retained moved/lost/unproven images.
- `session/image_build.go`, composition/reset/start/submission integration:
  mutation lock, running session/current durable attempt validation, declared
  image observation independent of functional grading; pending recovery blocks
  new starts and is visible through status/reset --recover.
- `cli/image.go`: `ckad image build --problem Q1 --context . --tag reference`.
  Runtime uses the recorded identity, preventing implicit environment recreation.
- `internal/imageexport`, grader image runner: exact native ID/tag proof before
  export and narrowly verified legacy fallback only for the API platform error.
  GraderVersion is bumped from grader-2 to grader-3 to invalidate certificates.
- Focused tests in those packages and `tests/integration/task14_images_test.go`;
  runtime ownership/limits/recovery documentation in `runtime/README.md`.

RED evidence (commands issued in the existing Linux development container):

1. `docker exec junkloud-remaining-dev go test ./internal/author -run '^TestScoped' -count=1`
   exited 1: `undefined: author.NewActionExecutor` (new executor API absent).
2. `docker exec junkloud-remaining-dev go test ./internal/environment -run '^TestImageBuild' -count=1`
   exited 1: `undefined: ValidateBuildContext`, `undefined: OperationImageBuild`,
   `m.BuildImage undefined` (new bounded builder absent).
3. `docker exec junkloud-remaining-dev go test ./internal/imageexport -count=1`
   exited 1: `undefined: Save` (shared native export absent).
4. `docker exec junkloud-remaining-dev go test ./internal/author -run '^TestValidationStagesShare' -count=1`
   exited 1 with behavioral rejection:
   `solution: action 1 (file): environment: action kind not supported yet: problem validation cannot run "file" actions yet`.
5. `docker exec junkloud-remaining-dev go test ./internal/session -run '^TestManagedImageBuild' -count=1`
   exited 1: `undefined: session.ImageBuildOptions`, `h.svc.BuildImage undefined`.
6. `docker exec junkloud-remaining-dev go test ./internal/grader -run '^TestImageRunDocker24VerifiedExport$' -count=1`
   exited 1 with behavioral failure:
   `save the image: docker save --platform failed (exit 1): "--platform" requires API version 1.48, but the Docker daemon API version is 1.43`.
7. `docker exec junkloud-remaining-dev go test ./internal/cli -run '^TestImageBuildCLI$' -count=1`
   exited 1: actual CLI `image build ...` returned usage exit 2 instead of 0.

These were feature failures, not missing Docker-daemon failures. API-absence
REDs are explicitly distinguished from behavioral REDs. During implementation,
compiler diagnostics exposed a shadowed `provided` name and missing imports;
those were corrected. Image-run unit assertions were updated to permit the
required read-only inspection and immutable-ID tagging.

GREEN evidence so far:

- Focused author/environment/imageexport command with
  `-run 'TestScoped|TestImageBuild|TestNativeExport' -count=1`: all three `ok`.
- `go test ./internal/author -count=1`: `ok .../author 4.644s`.
- Managed attempt-boundary test: `ok .../session 0.643s`.
- Earlier full affected-package run (before final boundary test additions):
  author `8.120s`, environment `4.296s`, session `79.133s`, cli `13.499s`,
  grader `5.692s`, imageexport `0.008s`, all `ok`, exit 0.
- `docker exec junkloud-remaining-dev go test -tags=integration ./tests/integration -run '^$' -count=1`:
  `ok .../tests/integration 0.020s [no tests to run]`; compilation only.
- `git diff --check`: exit 0 after trimming a trailing README blank line.
- Final code-freeze affected-package command:
  `docker exec junkloud-remaining-dev go test ./internal/author ./internal/environment ./internal/session ./internal/cli ./internal/grader ./internal/imageexport -count=1`
  exited 0: author `7.714s`, environment `4.645s`, session `69.767s`,
  cli `12.742s`, grader `5.656s`, imageexport `0.011s`, all `ok`.
- Live raw-log path and outcome will be appended below.

Live acceptance plan, code frozen before execution:

`docker exec junkloud-remaining-dev env JUNKLOUD_TEST_HOST_COMPAT=no-ipv6 go test -tags=integration ./tests/integration -run '^TestTask14ImageWorkflow$' -count=1 -timeout=20m -v`

The test creates an isolated real kind environment and temporary bank (one
image plus four tiny file problems), certifies every problem using the real
Validator/engine, prepares/starts a real practice session, and calls `cli.Run`
for two managed build/rebuild/grade/export/grade/reset cycles. It verifies
previous IDs disappear, archived personal files remain, direct Docker images
can get full functional marks, unlabelled cleanup retains an ownership error
and blocks starts, explicit exact-ID manual removal permits recorded recovery,
and an unrelated foreign sentinel tag retains the same image ID.

Concerns and limits:

- Native M2 and the Docker VM 4-GiB target: NOT RUN. Host is Windows/WSL2,
  Docker24/API1.43, actual `no-ipv6` compatibility only.
- A managed rebuild first removes the proven previous image. A failing new
  build leaves a durable intent; recovery clears only proven artifacts, then
  the learner rebuilds. It does not retain an automatic rollback image.
- Context bounds are 32 MiB/4096 entries; one FROM stage, local COPY, supported
  Dockerfile metadata/RUN. ADD, external stages/frontends and isolation override
  flags are rejected. File/image setup is deliberately unsupported.
- Functional grading never requires ownership labels. Automatic deletion does;
  direct images and moved/shared/unproven references retain errors/records.
- Docker build caches are not pruned; owned image IDs/tags are what reset owns.
- Source content is never included in build failure diagnostics. Private
  author sources are not copied into the initial/reset learning workspace.

Self-review: checked mutation ordering, exact owner/ID/tag checks, pending
recovery, preserved Task13 durable attempt minimum/history, direct grading
separation, export fallback restrictions and exclusion of controller plan files.
