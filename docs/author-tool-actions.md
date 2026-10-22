# Local author deployment actions

Validation can execute reviewed local deployment fixtures with
`NewActionExecutorWithTools` and `ToolActionConfig`. The existing
`NewActionExecutor` keeps Helm and Kustomize unsupported. `NewRuntime` and the
bank validation helper supply the verified environment's runner, reader,
kubeconfig and context.

Supported private solution/case actions:

```yaml
kind: helm
args:
  operation: install
  release: answer
  chart: chart
  values: [values.yaml]
```

```yaml
kind: kustomize
args: {path: overlay}
```

Paths refer to the current stage workspace. Earlier file actions affect the
render; their private source bytes remain raw. Each action retains its reviewed
snapshot through execution, so later file actions cannot change its inputs.
The complete action list is checked before stage-file, image or cluster effects.
Helm and Kustomize setup actions cannot be certified: learner setup uses the
existing apply/delete-owned path.

Helm author charts are local application charts with `Chart.yaml`, optional
`values.yaml`, and YAML files directly under `templates/`. Their template AST
permits literal text/scalars, `.Values.*`, `.Release.Name`, `.Release.Namespace`,
and an optional `quote` pipeline. Control flow, template definitions/invocation,
other functions/objects, dependencies, vendored charts, hooks, CRDs and lifecycle
annotations are refused. Ordinary learner Helm install/upgrade is unchanged.
The same bytes, values, release and namespace feed native template and install;
the restricted AST prevents cluster capabilities/lookup/time from changing the
second render. Native install creates real Helm release state without force,
replacement, ownership takeover or namespace creation.

Kustomize uses pinned `kubectl kustomize` with `LoadRestrictionsRootOnly`, then
applies decoded objects through the normal Manager. The shared local graph guard
checks resource/base/component, patch, generator file/env, replacement and
configuration paths before rendering. Remote inputs, external generators or
transformers, Helm inflation and external schema loaders are unsupported.

There are at most 8 native actions per list, 4 Helm values files per action,
4096 workspace entries and 32 MiB per snapshot, 128 rendered objects per action,
16 MiB per output stream, a 2-minute render-list budget and a 2-minute native
call budget. Accepted namespaced kinds are ConfigMap, Secret, Service,
ServiceAccount, Pod, Deployment, StatefulSet, DaemonSet, Job and CronJob;
all tool objects must stay in the problem namespace. Static validation checks
provided images using the installation's pinned image manifest.

Native effects require the exact previously persisted namespace UID and owner
marks. That identity is checked before effects and immediately before each
native action. Helm also rejects existing release storage or object collisions.
Namespace creation already journals intent and then its UID before an author
solution begins. After interruption, validation restores that persisted parent
identity before cleanup; the verified parent namespace reclaims Helm release
Secrets and native children. Missing identities and foreign replacements block
recovery and retain the journal/inventory. Cleanup never adopts a parent by
matching labels alone.
