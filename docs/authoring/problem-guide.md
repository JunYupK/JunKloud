# Problem authoring guide

How to write a JunKloud CKAD problem package and get it certified. This guide collects the rules that the bank packages (Tasks 14-18) were written under, and the validation cycle that decides whether a package can be served. Related documents:

- [`docs/author-tool-actions.md`](../author-tool-actions.md): the Helm and Kustomize author actions.
- [`schemas/problem-v1.schema.json`](../../schemas/problem-v1.schema.json): the authoritative schema (known criterion and action kinds only; no arbitrary code or shell).
- Design spec sections 6 and 7 ([spec](../superpowers/specs/2026-10-14-ckad-training-system-design.md)).

Questions are written in **English**; explanations and per-criterion feedback are written in **Korean**.

## 1. Package layout

```text
problems/<problem-id>/
  problem.yaml        # metadata, parameters, criteria, setup/solution/cases actions, initial score
  question.en.md      # the English task
  explanation.ko.md   # Korean: key concepts, diagnostic order, solution, official docs
  setup/              # initial resources; setup/provided/ is handed to the learner's attempt directory
  solution/           # the model solution (resources and files)
  cases/              # representative wrong answers and alternative correct answers
```

`problem.yaml` is a single JSON-compatible document (the bank files are written as JSON). Required fields: `schema_version` (1), `id`, `version`, `title`, `domain`, `difficulty`, `type`, `competencies`, `namespace`, `budget`, `criteria`, `solution`, `initial_expected`, `initial_score`. Usual optional fields: `tags`, `capabilities`, `parameters`, `scope`, `sources`, `setup`, `cases`.

- `domain`: `design-build`, `deployment`, `observability`, `environment-security`, `networking`.
- `difficulty`: `easy`, `medium`, `hard`. `type`: `build`, `modify`, `troubleshoot`, `behavioral`.
- `parameters`: named value lists (for example `namespace: [jk-<id>-a, jk-<id>-b]`). Give at least two namespace values so that `author.CoveringSeeds` returns at least two seeds. `${namespace}` and other parameters are substituted everywhere (setup, criteria, cases).
- `capabilities`: environment capabilities the problem needs (`image-build`, `storage`, `metrics`, `ingress`, `network-policy`, ...). A problem whose capability is not PASS is not served, and a mock exam containing it is refused as a whole.
- `budget`: a small `cpu`/`memory` budget. Workloads use only the provided pinned images (`docker.io/library/busybox:1.37.0` for most problems).
- `initial_expected` / `initial_score`: the per-criterion result and score of the untouched initial state. A fully solved initial state is invalid.
- `cases`: each case has `id`, `actions`, `expected` (per-criterion PASS/FAIL) and `expected_score`.

Learner-visible content is the English question and `setup/provided/` files only. Criteria, the model solution, cases and the explanation are never exported to the attempt directory.

## 2. Criteria

Every criterion has `id`, `kind`, `points` (positive), `feedback` (Korean) and usually `target`, `args`, `timeout`, `depends_on`. The sum of the points must equal the problem's score.

| Kind | What it checks |
| --- | --- |
| `resource`, `field` | An object exists (GVK, namespace, name) and fields have the required values, compared by meaning (types and quantities, not strings) |
| `ready` | A workload reached the declared rollout and ready/available state, with a stability window and timeout |
| `http`, `tcp` | A real connection from a declared source to the destination; HTTP checks the status code and an application marker |
| `network-policy` | Allow and deny paths are each exercised against a healthy server |
| `endpoint-membership` | EndpointSlice membership of a Service |
| `logs`, `file` | Log lines or output files of the right shape |
| `image` | The image or archive exists and is actually run |
| `helm`, `kustomize` | The release or rendered overlay and the deployed state |
| `job`, `cronjob-template`, `cronjob-schedule` | Job results and CronJob template or schedule |
| `authorization` | The RBAC outcome (allowed and denied actions) |
| `security-context` | SecurityContext settings and, when required, the behaviour inside the container |
| `volume-persistence` | Data survives a Pod replacement |

Action kinds in `setup`, `solution` and `cases`: `apply`, `delete-owned`, `helm`, `kustomize`, `image`, `file`, `patch`. Paths stay inside the package; `file` sources are private package paths.

## 3. Authoring rules

These are binding. Each came from a review finding in an earlier task.

1. **Every criterion maps to a sentence in `question.en.md`.** Never grade a hidden condition. A sentence that says "do not change X" is graded, or it is not written. If a "do not change" clause is not graded, delete the sentence (this happened in `configuration-001` and `resources-001`).
2. **Wrong cases are server-accepted states.** A removed label, a loosened policy, a deleted quota are fine. An action that admission or quota rejects (403/422) aborts validation, so it never belongs in a case.
3. **Only `alternative-*` cases may score full marks.** They are the representative alternative correct answers (for example `alternative-split-roles`, `alternative-match-expressions`).
4. **`depends_on` instead of ERROR.** Never let a `ready` or `http` criterion meet a state where admission or quota blocks Pod creation: "no pods" is ERROR. Give such criteria a `depends_on` on a `field` or `security-context` (declared) criterion that fails first, so they FAIL as `prerequisite not satisfied`. Network criteria also `depends_on` the target workload's `ready` criterion, because EndpointSlices are read once with no waiting.
5. **A case that removes fields from a setup-created object uses `delete-owned` and then `apply`.** Setup creates objects with Create, so Create-owned fields cannot be removed by a later server-side apply.
6. **Stand-in versus real certificates.** A stand-in certificate (current problem hash, grader version, environment hash) lets a test get past the certificate check. It is lifecycle evidence only, never package validation. Never mark a problem validated from a stand-in or from a host that cannot run it (for example `networkpolicy-001` where NetworkPolicy is not enforced).
7. **Secret-reading criteria and redaction.** Secret values never appear in evidence, reasons or CLI text. The redactor only learns a Secret that some criterion reads, so a problem that involves a Secret needs a Secret-reading criterion with no `depends_on` (for example `secret-unchanged`, a `field` on the Secret) and a case where the secret value is used as a key (`secret-value-as-key`), so that the leak guard can prove nothing leaks.
8. **Timeouts and stability windows are stated in the question.** Every `http` criterion declares an explicit timeout (60s is the bank standard; network criteria use at least 45s, 60s for an Ingress target). Private `file` sources contain no `${`.
9. **Network checks are fair.** `network-policy` criteria include at least one allow and one deny, and every denied port has an allow on the same port. Check Pods follow the learner's selectors and namespaces; the platform adds no hidden policy. A dead server that blocks everything is not a correct answer. Cross-namespace sources need `scope.namespaces`.
10. **Least privilege needs explicit statements.** If least privilege is graded, the question lists the allowed and denied actions. Over-privilege must lose points even when everything works.
11. **Learner-created cluster-scoped objects are not owned** and survive reset, so questions say to use namespaced objects (except a provided CRD).
12. **Scope.** Declare the namespaces and cluster-scoped objects the package touches (`scope`). Objects outside it are rejected.
13. **Do not weaken a grader to make a case pass.** If a planted wrong answer scores unexpectedly, fix the grader or the case, never the expectation.

## 4. The validation cycle

```sh
ckad bank validate <problem-id>
```

Run inside the work container, with no active practice or mock session (validation refuses to run next to one, and next to another prepare/grade/validate). The cycle for one package:

1. Static schema and content checks (known kinds, ids, points, parameter references, paths, provided images).
2. A temporary problem environment is built for each covering seed.
3. **Initial:** the untouched state scores `initial_score` with exactly `initial_expected`.
4. **Solved:** after the model solution every criterion passes and the score is full.
5. **Cases:** after each case's actions, exactly the `expected` criteria fail and the score equals `expected_score`; alternative cases score full.
6. **Reset:** the environment returns to the initial state without leftovers from the previous run.
7. On success a certificate is recorded for this exact (problem hash, grader version, environment hash) and the CLI prints `검증 완료: <id> | <N>개 seed | hash <hash>`.

A certificate is stale as soon as any of the three changes. In particular, the grader version is part of the key (currently `grader-9`), so every grader change requires re-validating the bank. The bank test suites under `tests/integration` (`TestBuildBank`, `TestDeploymentBank`, `TestObservabilityBank`, `TestSecurityBank`, `TestNetworkBank`, each with a `...Smoke` variant) run the same cycle for a whole domain, with leak guards for Secret values. Run the smoke variant on frozen source first, then the full one; a single package: `-run '^TestNetworkBank$/^networkpolicy-001$'`.

Time and host notes:

- Validation takes real cluster time and the per-problem duration has not been measured on a device.
- A host that cannot enforce a capability fails the affected package honestly (its solved stage fails). It is reported as not certified, and the package is not edited to match.
- On a host that cannot enforce NetworkPolicy, set `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy` for the test suites and expect `networkpolicy-001` to be skipped, never certified.

## 5. Checklist before you send a package

- [ ] Each question sentence is graded, and each criterion is in the question.
- [ ] Criteria points sum to the problem's score; `initial_expected` and `initial_score` are exact.
- [ ] At least two parameter values; `${...}` only where intended; no `${` in private `file` sources.
- [ ] Wrong cases are server-accepted; only `alternative-*` score full marks; field-removing cases use `delete-owned` then `apply`.
- [ ] Prerequisite chains use `depends_on`; no criterion can see "no pods" as ERROR.
- [ ] Secret handling has a Secret-reading criterion and a leak-guard case.
- [ ] Questions in English, feedback and explanation in Korean.
- [ ] `ckad bank validate <id>` passes on a host that supports the package's capabilities.
