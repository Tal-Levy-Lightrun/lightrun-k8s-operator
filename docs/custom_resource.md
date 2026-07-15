## Two mechanisms

This operator currently supports two ways to inject the Lightrun Java agent into a pod:

1. **`AgentPool`/`ClusterAgentPool` CRDs + Pod annotations** (current, recommended) — a mutating
   admission webhook injects the agent into any pod carrying (or inheriting, at the namespace
   level) an `lightrun.com/inject-java` annotation, at pod creation time. No CR to create per
   workload, no controller reconcile loop, works regardless of the owning workload kind.
2. **`LightrunJavaAgent` CRD** (legacy) — a controller reconciles a `LightrunJavaAgent` CR and
   patches one named Deployment/StatefulSet. Still fully functional and still supported for
   existing users, but being phased out in favor of the webhook/annotation mechanism above. New
   installations should prefer the `AgentPool`/annotation approach.

Both mechanisms can run side by side in the same cluster/operator install; enabling the webhook
does not disable or remove the `LightrunJavaAgent` controller.

---

## Quick start: the zero/minimal-annotation path (recommended)

The headline goal of the webhook mechanism is that most pods need **zero or one annotation** —
not a full set of ~8 required annotations repeated on every workload. The recommended pattern:

1. Create one `AgentPool` named `default` in a namespace, holding the shared config every
   workload in that namespace should inherit (credentials, hostname, container to patch, env
   var, tags, etc.):

   ```yaml
   apiVersion: agents.lightrun.com/v1beta
   kind: AgentPool
   metadata:
     name: default
     namespace: lightrun-agent-test
   spec:
     secretRef:
       name: lightrun-secrets
     serverHostname: app.lightrun.com
     containerSelector: ["app"]
     agentEnvVarName: JAVA_TOOL_OPTIONS
     agentTags: ["production"]
   ```

2. Opt the namespace in **once**, by setting the same trigger annotation on the `Namespace`
   object itself:

   ```yaml
   apiVersion: v1
   kind: Namespace
   metadata:
     name: lightrun-agent-test
     annotations:
       lightrun.com/inject-java: "true"
   ```

That's it. **Every pod created in that namespace from now on — including ones from
Deployments/Helm charts you don't want to touch — gets the agent injected, with zero
per-pod annotations.** `"true"` resolves to the `AgentPool` literally named `default` in the
pod's own namespace (see [Trigger annotation](#trigger-annotation-lightruncominject-java)
below).

Only deviate from this when a specific workload genuinely needs something different:

- A pod that needs a **different pool** than the namespace default needs exactly **one**
  annotation: `lightrun.com/inject-java: <pool-name>`.
- A pod that needs to **opt out** needs exactly one annotation: `lightrun.com/inject-java: "false"`.
- A pod that needs to override **one field** (e.g. its own `agent-tags`) needs that one
  per-field override annotation on top of everything else still coming from the resolved pool —
  never the full annotation set. See
  [Per-pod override annotations](#per-pod-override-annotations) below.

The fully-manual, no-namespace-default, every-annotation-on-every-pod approach documented further
down still works and is sometimes the right choice (e.g. no single shared pool makes sense for a
namespace) — but it's the fallback/advanced option, not the recommended starting point.

---

## `AgentPool` CRD (namespaced)

`AgentPool` is namespaced. An `AgentPool`, the `Secret` it references, and the pods that resolve
to the pool must all live in the **same namespace** — this keeps the Secret, the `AgentPool` CR,
and the consuming Pod inside one ordinary Kubernetes RBAC boundary, with no new write RBAC and no
mirroring. If you need to share credentials/config across multiple namespaces without duplicating
them per namespace, use a [`ClusterAgentPool`](#clusteragentpool-crd-cluster-scoped) instead.

```yaml
apiVersion: agents.lightrun.com/v1beta
kind: AgentPool
metadata:
  name: example-pool
  namespace: lightrun-agent-test
spec:
  # Name of a Secret in the *same namespace* as this AgentPool, holding the
  # lightrun_key / pinned_cert_hash keys (same keys the legacy mechanism uses).
  secretRef:
    name: lightrun-secrets
  # Hostname of the Lightrun server. For SaaS it's app.lightrun.com; for
  # on-prem/single-tenant installations, use your own hostname.
  serverHostname: app.lightrun.com

  # Everything below is a SHARED DEFAULT for every pod that resolves to this pool. Each one can
  # still be overridden per-pod via the matching lightrun.com/* annotation -- see
  # "Per-pod override annotations" below. All are optional; a pod (or another field further
  # down this same list) can supply what's missing.
  containerSelector: ["app"]
  agentEnvVarName: JAVA_TOOL_OPTIONS
  agentCliFlags: ""
  agentTags: ["operator", "example"]
  agentName: ""
  agentConfig:
    max_log_cpu_cost: "2"
  useSecretsAsMountedFiles: true
---
apiVersion: v1
kind: Secret
metadata:
  name: lightrun-secrets
  namespace: lightrun-agent-test
stringData:
  lightrun_key: <lightrun_key_from_ui>
  pinned_cert_hash: <pinned_cert_hash>
type: Opaque
```

| Field                            | Required | Description                                                                                                                  |
| --------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------- |
| `spec.secretRef.name`             | Yes      | Name of the `Secret` (same namespace as the `AgentPool`) holding `lightrun_key`/`pinned_cert_hash`.                          |
| `spec.serverHostname`             | Yes      | Lightrun server hostname the agent connects to (`app.lightrun.com` for SaaS).                                                |
| `spec.containerSelector`          | No       | Default list of container names to patch. Overridable per-pod via `lightrun.com/container-selector`.                        |
| `spec.agentEnvVarName`            | No       | Default env var the `-agentpath:...` argument is appended to (e.g. `JAVA_TOOL_OPTIONS`). Overridable via `lightrun.com/agent-env-var-name`. |
| `spec.agentCliFlags`              | No       | Default extra CLI flags. Overridable via `lightrun.com/agent-cli-flags`.                                                     |
| `spec.agentTags`                  | No       | Default tags, visible in the Lightrun UI/IDE plugin. Overridable via `lightrun.com/agent-tags`.                              |
| `spec.agentName`                  | No       | Default display name. Overridable via `lightrun.com/agent-name`.                                                             |
| `spec.agentConfig`                | No       | Default map of string key/value pairs overriding agent config. Overridable via `lightrun.com/agent-config`.                 |
| `spec.useSecretsAsMountedFiles`   | No       | Default mount-as-volume-vs-env behavior for the Secret. Overridable via `lightrun.com/use-secrets-as-mounted-files`. Falls through to `true` if never set anywhere. |

If the named `AgentPool` doesn't exist in the pod's namespace, or its `spec.secretRef.name` is
empty, the webhook **denies admission of the pod** with an error naming the missing pool/field —
the pod is not created.

## `ClusterAgentPool` CRD (cluster-scoped)

`ClusterAgentPool` is cluster-scoped: it lets pods across many namespaces share one pool's
config/credentials without an admin having to create a separate `AgentPool` + `Secret` in every
namespace. Its spec fields mirror `AgentPool`'s shared-default fields exactly (same names,
same per-field pod-annotation overrides), plus two additions that make cross-namespace sharing
actually work:

- `spec.secretRef` is `{name, namespace}` — the **source** Secret's location (e.g. the operator's
  own namespace, or a platform-team namespace). Unlike `AgentPool`, this is *not* the same
  namespace as the consuming pod — see [How the Secret gets to the pod's namespace](#how-the-secret-gets-to-the-pods-namespace)
  below for why that's not a problem.
- `spec.allowedNamespaces` is an explicit allow-list of namespaces permitted to use this pool.
  **Secure by default: empty/absent means usable by nothing.** There is no wildcard or
  label-selector support — an explicit list is the simplest thing to reason about and audit.

```yaml
apiVersion: agents.lightrun.com/v1beta
kind: ClusterAgentPool
metadata:
  name: platform-default
spec:
  secretRef:
    name: lightrun-secrets
    namespace: lightrun-operator
  serverHostname: app.lightrun.com
  allowedNamespaces:
    - team-a
    - team-b
  containerSelector: ["app"]
  agentEnvVarName: JAVA_TOOL_OPTIONS
  agentTags: ["platform-managed"]
```

| Field                             | Required | Description                                                                                                              |
| ---------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------- |
| `spec.secretRef.name`              | Yes      | Name of the source `Secret` holding `lightrun_key`/`pinned_cert_hash`.                                                    |
| `spec.secretRef.namespace`         | Yes      | Namespace the source `Secret` lives in — does **not** need to match any consuming pod's namespace.                        |
| `spec.serverHostname`              | Yes      | Lightrun server hostname the agent connects to.                                                                          |
| `spec.allowedNamespaces`           | No*      | Explicit allow-list of namespaces permitted to use this pool. Empty/absent = usable by nothing. *Effectively required — without it, no pod anywhere can use this pool. |
| `spec.containerSelector`           | No       | Same as `AgentPool`.                                                                                                     |
| `spec.agentEnvVarName`             | No       | Same as `AgentPool`.                                                                                                     |
| `spec.agentCliFlags`               | No       | Same as `AgentPool`.                                                                                                     |
| `spec.agentTags`                   | No       | Same as `AgentPool`.                                                                                                     |
| `spec.agentName`                   | No       | Same as `AgentPool`.                                                                                                     |
| `spec.agentConfig`                 | No       | Same as `AgentPool`.                                                                                                     |
| `spec.useSecretsAsMountedFiles`    | No       | Same as `AgentPool`.                                                                                                     |

A pod resolves to a `ClusterAgentPool` by naming it via `lightrun.com/inject-java: <name>` (or a
namespace-level default of the same form) plus `lightrun.com/agent-pool-kind: ClusterAgentPool` —
see [Trigger annotation](#trigger-annotation-lightruncominject-java) below. If the pod's namespace
isn't listed in `allowedNamespaces`, the webhook **denies admission** with a
"namespace not permitted to use this ClusterAgentPool" error — this is the multi-tenant safety
gate.

### How the Secret gets to the pod's namespace

A core Kubernetes `Secret` volume/`secretKeyRef` can only ever reference a Secret **in the same
namespace as the pod** — there is no way to mount a Secret across namespaces directly. So a
`ClusterAgentPool`'s credential Secret is never mounted cross-namespace into a consuming pod.
Instead, it is **mirrored**:

- A dedicated controller (`internal/controller/clusteragentpool.SecretMirrorReconciler`, run as
  a normal controller alongside the rest of the operator, *not* inline in the webhook's hot
  admission path) watches every `ClusterAgentPool` and its source Secret.
- For every namespace listed in `spec.allowedNamespaces`, it creates (and keeps in sync,
  including on credential rotation) a **copy** of the source Secret in that namespace, under the
  deterministic name `<clusterAgentPool-name>-mirror`, labeled
  `lightrun.com/cluster-agent-pool: <clusterAgentPool-name>` and owned (via `ownerReference`) by
  the `ClusterAgentPool` — so deleting the `ClusterAgentPool` garbage-collects all its mirrors
  automatically.
- The webhook, once it's confirmed the pod's namespace is allowed, references this **local
  mirrored Secret** (by its deterministic name, in the pod's own namespace) exactly the way it
  would reference an `AgentPool`'s own Secret — never the cross-namespace source directly.
- If a namespace is later removed from `allowedNamespaces`, the controller deletes the
  now-stale mirror from that namespace on the next reconcile — access is actually revoked, not
  just gated at admission time going forward.
- If something else already occupies the deterministic mirror name in a target namespace (and
  isn't already labeled as this pool's own mirror), the controller **refuses to adopt or
  overwrite it** rather than silently clobbering an unrelated Secret — the refusal is visible via
  the `ClusterAgentPool`'s `SecretMirrorSynced` status condition, a Warning Event, and a returned
  reconcile error/log line.

**In short: your Secret is copied into each allowed namespace, not shared/cross-mounted.** Each
namespace ends up with its own local Secret object (kept in sync automatically), which is what
actually gets mounted into pods there.

---

## Pod annotation contract

All annotations live under the `lightrun.com/` prefix, on the Pod (or Pod template
— `spec.template.metadata.annotations` on a Deployment/StatefulSet).

### Trigger annotation: `lightrun.com/inject-java`

This is the primary trigger. Value forms:

| Value                        | Behavior                                                                                                                            |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| *(absent on the pod)*          | Inherit the [namespace-level default](#namespace-level-default) (see below). If there's no namespace default either, no injection — pod admitted unchanged. |
| `"false"`                     | Explicit opt-out. Overrides any namespace-level default.                                                                              |
| `"true"`                      | Inject using the pool named literally `default`: tries a namespaced `AgentPool` named `default` in the pod's own namespace first, then a `ClusterAgentPool` named `default` cluster-wide. Neither exists → **DENY**. |
| `"<name>"`                    | Inject using the pool named `<name>`, of kind `lightrun.com/agent-pool-kind` (optional, default `AgentPool`; set to `ClusterAgentPool` to resolve a cluster-scoped pool instead). |

`lightrun.com/agent-type` still exists, but now only as an **optional override** of *which* agent
type to dispatch to (defaulting to `"java"`, the only implemented type today, when absent) — it
is no longer a required trigger by itself when used alongside `lightrun.com/inject-java`. If it's
set to an unrecognized value (e.g. a not-yet-implemented `"nodejs"`/`"python"`), the pod is
admitted unchanged even if `lightrun.com/inject-java` names a valid pool.

### Namespace-level default

Set the exact same `lightrun.com/inject-java` annotation on the **Namespace** object itself (the
webhook does a live read of the pod's own Namespace). It applies to every pod in that namespace
that omits its own `lightrun.com/inject-java` annotation, using the same value-form rules as
above (`"true"`/`"false"`/`"<name>"`). This is the main lever for "opt in once, annotate nothing
per-pod" — see the [Quick start](#quick-start-the-zerominimal-annotation-path-recommended) above.

### Legacy trigger (backward compatible)

The pre-v3 trigger still works unchanged: `lightrun.com/agent-type` present with a non-empty,
recognized value (currently just `"java"`) is itself the trigger, and
`lightrun.com/agent-pool` (always a namespaced `AgentPool`, never a `ClusterAgentPool`) names the
pool — required once this trigger fires; a missing `lightrun.com/agent-pool` in this case is a
**DENY** ("missing required annotation"), not a fall-through to the namespace default.

### Precedence

Highest to lowest, evaluated per pod:

1. The pod's own `lightrun.com/inject-java` annotation (new-style trigger).
2. The pod's own legacy-style trigger (`lightrun.com/agent-type` + `lightrun.com/agent-pool`).
3. The namespace-level `lightrun.com/inject-java` default.
4. No injection.

In other words: **a pod's own explicit trigger — either style — always wins over a namespace
default.** A namespace admin turning on a namespace-level default can never silently redirect an
already-explicitly-configured pod's credentials/server to a different pool; it only fills in for
pods that set no trigger of their own.

### Per-pod override annotations

Everything below is resolved from the pool ([`AgentPool`](#agentpool-crd-namespaced) or
[`ClusterAgentPool`](#clusteragentpool-crd-cluster-scoped)) determined via the trigger above, then
**merged per-field** with the pod's own annotations: each annotation, if present on the pod,
overrides just that one field; if absent, that field falls through to the pool's value. This is
not all-or-once — a pod can override e.g. just its tags while every other field (secret,
hostname, container selector, env var, ...) still comes entirely from the pool.

| Annotation                                   | Default (when absent both here and on the pool) | Description                                                                                                   |
| --------------------------------------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `lightrun.com/agent-pool-kind`                | `AgentPool`                                        | Only consulted when `lightrun.com/inject-java` (or the namespace default) names a pool explicitly by name; ignored for `"true"` (which always tries both kinds under `default`). |
| `lightrun.com/container-selector`             | —                                                  | Comma-separated list of container names in the pod to patch. Resolving to zero names, or naming a container absent from the pod, is a **DENY**. |
| `lightrun.com/agent-env-var-name`             | —                                                  | Env var to append the agent's `-agentpath:...` argument to, e.g. `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `JAVA_OPTS`, `MAVEN_OPTS`. |
| `lightrun.com/agent-cli-flags`                | (none)                                             | Extra CLI flags appended to the `-agentpath:...` argument. See [agent-configuration docs](https://docs.lightrun.com/jvm/agent-configuration/). |
| `lightrun.com/agent-tags`                     | (none)                                             | Comma-separated tags, visible in the Lightrun UI and IDE plugin. This is the canonical example of a genuinely per-workload field — a shared pool gives every pod a sane default, and a single workload can still override just its own tags without repeating the rest of the pool's config. |
| `lightrun.com/agent-name`                     | pod name                                           | Custom display name for the agent.                                                                            |
| `lightrun.com/agent-config`                   | `{}`                                                | JSON-encoded object of string key/value pairs overriding default agent config, e.g. `{"max_log_cpu_cost":"2"}`. See [agent-flags docs](https://docs.lightrun.com/jvm/agent-configuration/#agent-flags). |
| `lightrun.com/use-secrets-as-mounted-files`   | `true`                                              | `"true"`/`"false"`. When true, the pool's Secret is mounted as a volume into the init container; when false, its keys are injected as env vars instead. |

Example — a pod in a namespace with no default pool configured, needing the full manual set
against a specific `AgentPool` (the fallback/advanced pattern from the Quick start above):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sample-deployment
spec:
  template:
    metadata:
      annotations:
        lightrun.com/inject-java: "example-pool"
        lightrun.com/container-selector: "app"
        lightrun.com/agent-env-var-name: "JAVA_TOOL_OPTIONS"
        lightrun.com/agent-tags: "operator,example"
        lightrun.com/agent-config: '{"max_log_cpu_cost":"2"}'
    spec:
      containers:
        - name: app
          image: my-app:latest
```

Example — the same pod, but the namespace already has an `agentEnvVarName`/`containerSelector`
default pool, and this workload only needs its own tags:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sample-deployment
spec:
  template:
    metadata:
      annotations:
        lightrun.com/agent-tags: "operator,example"
    spec:
      containers:
        - name: app
          image: my-app:latest
```

### Misconfiguration / DENY behavior

The webhook **admits the pod unchanged** when no trigger fires at all — the pod itself sets
neither `lightrun.com/inject-java` nor the legacy `lightrun.com/agent-type`+`lightrun.com/agent-pool`
pair, and the pod's namespace has no `lightrun.com/inject-java` default either — or when
`lightrun.com/agent-type` resolves to an unrecognized value (e.g. a not-yet-implemented
`"nodejs"`/`"python"`).

Once a trigger fires, the webhook **denies admission of the pod** (the pod is not created) in
each of the following cases:

- `lightrun.com/inject-java: "true"` and no `AgentPool` or `ClusterAgentPool` named `default`
  exists (checked namespaced first, then cluster-wide).
- The named `AgentPool`/`ClusterAgentPool` doesn't exist (in the pod's namespace, for `AgentPool`;
  cluster-wide, for `ClusterAgentPool`), or its `spec.secretRef.name` is empty.
- The legacy trigger fires but `lightrun.com/agent-pool` is missing.
- The resolved pool is a `ClusterAgentPool` and the pod's namespace is not listed in its
  `spec.allowedNamespaces`.
- The merged `lightrun.com/container-selector` resolves to zero container names (e.g. it's set to
  an empty or all-whitespace string).
- One or more names in the merged container selector don't match any container in the pod spec
  ("unable to find matching container to patch").
- The merged `lightrun.com/agent-config` isn't valid JSON, or isn't a JSON object of strings.
- The merged `lightrun.com/use-secrets-as-mounted-files` isn't a valid bool.
- The resulting agent argument (`-agentpath:<mountPath>/agent/lightrun_agent.so[=<agentCliFlags>]`)
  combined with any pre-existing value of the target env var exceeds **1024 characters** — a hard
  limitation of Java, not configurable.

Note: this per-pod DENY behavior is independent of the webhook's admission `failurePolicy`
(`Ignore` by default — see the [chart README](../charts/lightrun-operator/README.md)), which only
governs what happens if the webhook *service itself* is unreachable.

---

## `LightrunJavaAgent` CRD (legacy)

> **Legacy mechanism.** This is the original CR-and-controller-based way to inject the Lightrun
> Java agent: you create a `LightrunJavaAgent` CR naming a specific Deployment/StatefulSet, and a
> controller reconciles it by patching that workload. It is still fully functional and supported
> for existing users, but new installations should prefer the
> [`AgentPool`/`ClusterAgentPool`/Pod-annotation mechanism](#quick-start-the-zerominimal-annotation-path-recommended)
> above. See [how.md](how.md) for how the two mechanisms differ operationally.

```yaml
apiVersion: agents.lightrun.com/v1beta
kind: LightrunJavaAgent
metadata:
  name: example-cr 
spec:
  # Init container with agent. Differes by agent version and platform that it will be used for. For now supported platforms are `linux` and `alpine`  
  initContainer:  
    # parts that may vary here are 
    # platform - `linux/alpine`
    # agent version - first part of the tag (1.7.0)
    # init container sub-version - last part of the tag (init.0)
    image: "lightruncom/k8s-operator-init-java-agent-linux:1.7.0-init.0"
    # imagePullPolicy of the init container. Can be one of: Always, IfNotPresent, or Never.
    imagePullPolicy: "IfNotPresent"
    # Volume name in case you have some convention in the names
    sharedVolumeName: lightrun-agent-init
    # Mount path where volume will be parked. Various distributions may have it's limitations.
    # For example you can't mount volumes to any path except `/tmp` when using AWS Fargate
    sharedVolumeMountPath: "/lightrun"
  # Name of the workload that you are going to patch.
  # Has to be in the same namespace
  workloadName: app
  # Type of the workload that you are going to patch.
  # Has to be one of `Deployment` or `StatefulSet`
  workloadType: Deployment
  # Name of the secret where agent will take `lightrun_key` and `pinned_cert_hash` from
  # Has to be in the same namespace
  secretName: lightrun-secrets 
  # Hostname of the server. Will be different for on-prem ans single-tenant installations
  # For saas it will be app.lightrun.com
  serverHostname: <lightrun_server>  
  # Env var that will be patched with agent path.
  # If your application not using any, recommended option is to use "JAVA_TOOL_OPTIONS"
  # Also may be "_JAVA_OPTIONS", "JAVA_OPTS"
  # There are also different variations if using Maven -  "MAVEN_OPTS", and so on
  # You can find more info here: https://docs.lightrun.com/jvm/agent/
  agentEnvVarName: JAVA_TOOL_OPTIONS
  # Agent config will override  default configuration with provided values
  # You can find list of available options here https://docs.lightrun.com/jvm/agent-configuration/
  agentConfig:
    max_log_cpu_cost: "2"
  # Tags that agent will be using. You'll see them in the UI and in the IDE plugin as well
  agentTags:
    - operator
  # Agent name. If not provided, pod name will be used
  #agentName: "operator-test-agent"
  # List of container names inside the pod of the deployment
  # If container not mentioned here it will be not patched
  containerSelector:
    - app
  # useSecretsAsMountedFiles determines whether to use secret values as mounted files (true) or as environment variables (false)
  # Default is true
  useSecretsAsMountedFiles: false
---
apiVersion: v1
metadata: 
  name: lightrun-secrets
stringData:
  # Lightrun key you can take from the server UI at the "setup agent" step
  lightrun_key: <lightrun_key_from_ui>
  # Server certificate hash. It is ensuring that agent is connected to the right Lightrun server
  pinned_cert_hash: <pinned_cert_hash>
kind: Secret
type: Opaque
```
