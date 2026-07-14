## Two mechanisms

This operator currently supports two ways to inject the Lightrun Java agent into a pod:

1. **`AgentPool` CRD + Pod annotations** (current, recommended) — a mutating admission webhook
   injects the agent into any pod carrying the right `lightrun.com/*` annotations, at pod
   creation time. No CR to create per workload, no controller reconcile loop, works regardless
   of the owning workload kind.
2. **`LightrunJavaAgent` CRD** (legacy) — a controller reconciles a `LightrunJavaAgent` CR and
   patches one named Deployment/StatefulSet. Still fully functional and still supported for
   existing users, but being phased out in favor of the webhook/annotation mechanism above. New
   installations should prefer the `AgentPool`/annotation approach.

Both mechanisms can run side by side in the same cluster/operator install; enabling the webhook
does not disable or remove the `LightrunJavaAgent` controller.

---

## AgentPool CRD and Pod annotations (current, recommended)

### How targeting works

Instead of creating a CR that names a specific Deployment/StatefulSet, you annotate the **Pod
template** (`spec.template.metadata.annotations` on a Deployment/StatefulSet, or a bare Pod's own
`metadata.annotations`) with `lightrun.com/*` annotations. The pod-mutating webhook (see
[how.md](how.md)) inspects every Pod at admission time and injects the agent if, and only if, the
pod carries a recognized `lightrun.com/agent-type` annotation. The webhook must be enabled
(`webhook.enabled: true` in the Helm chart / `--webhook-enabled` on the manager) for this
mechanism to do anything — see the [chart README](../charts/lightrun-operator/README.md) for the
`webhook.*` values.

Sensitive credentials (the Lightrun key, pinned cert hash) are never placed in annotations. They
stay in a classic `Secret`, referenced indirectly through an `AgentPool` custom resource.

### `AgentPool` CRD

`AgentPool` is namespaced — there is no cluster-scoped equivalent. An `AgentPool`, the `Secret` it
references, and the pods that reference the pool must all live in the **same namespace**. If you
need to share one pool's credentials/hostname across multiple namespaces, create a separate
`AgentPool` + `Secret` in each namespace.

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

| Field                    | Required | Description                                                                                    |
| ------------------------ | -------- | ------------------------------------------------------------------------------------------------ |
| `spec.secretRef.name`    | Yes      | Name of the `Secret` (same namespace as the `AgentPool`) holding `lightrun_key`/`pinned_cert_hash`. |
| `spec.serverHostname`    | Yes      | Lightrun server hostname the agent connects to (`app.lightrun.com` for SaaS).                    |

If the named `AgentPool` doesn't exist in the pod's namespace, or its `secretRef.name` is empty,
the webhook **denies admission of the pod** with an error naming the missing pool/field — the pod
is not created.

### Pod annotations

All annotations live under the `lightrun.com/` prefix, on the Pod (or Pod template):

| Annotation                                   | Required        | Default | Description                                                                                                   |
| --------------------------------------------- | --------------- | ------- | --------------------------------------------------------------------------------------------------------------- |
| `lightrun.com/agent-type`                     | Yes             | —       | Must be `"java"` (the only implemented agent type today). This is the **trigger**: pods without a recognized value here are admitted unchanged — the webhook does not touch them. |
| `lightrun.com/agent-pool`                     | Yes*            | —       | Name of the `AgentPool` in the pod's own namespace to resolve credentials/server hostname from.                |
| `lightrun.com/container-selector`             | Yes*            | —       | Comma-separated list of container names in the pod to patch.                                                   |
| `lightrun.com/agent-env-var-name`             | Yes*            | —       | Env var to append the agent's `-agentpath:...` argument to, e.g. `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `JAVA_OPTS`, `MAVEN_OPTS`. |
| `lightrun.com/agent-cli-flags`                | No              | (none)  | Extra CLI flags appended to the `-agentpath:...` argument. See [agent-configuration docs](https://docs.lightrun.com/jvm/agent-configuration/). |
| `lightrun.com/agent-tags`                     | No              | (none)  | Comma-separated tags, visible in the Lightrun UI and IDE plugin.                                               |
| `lightrun.com/agent-name`                     | No              | pod name | Custom display name for the agent.                                                                            |
| `lightrun.com/agent-config`                   | No              | `{}`    | JSON-encoded object of string key/value pairs overriding default agent config, e.g. `{"max_log_cpu_cost":"2"}`. See [agent-flags docs](https://docs.lightrun.com/jvm/agent-configuration/#agent-flags). |
| `lightrun.com/use-secrets-as-mounted-files`   | No              | `true`  | `"true"`/`"false"`. When true, the pool's Secret is mounted as a volume into the init container; when false, its keys are injected as env vars instead. Any absent or unparseable value defaults to `true`. |

*Required only when `lightrun.com/agent-type` is a recognized value (currently just `"java"`).

Example:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sample-deployment
spec:
  template:
    metadata:
      annotations:
        lightrun.com/agent-type: "java"
        lightrun.com/agent-pool: "example-pool"
        lightrun.com/container-selector: "app"
        lightrun.com/agent-env-var-name: "JAVA_TOOL_OPTIONS"
        lightrun.com/agent-tags: "operator,example"
        lightrun.com/agent-config: '{"max_log_cpu_cost":"2"}'
    spec:
      containers:
        - name: app
          image: my-app:latest
```

### Misconfiguration / DENY behavior

The webhook **admits the pod unchanged** when `lightrun.com/agent-type` is absent or set to an
unrecognized value (e.g. a not-yet-implemented `"nodejs"`/`"python"`).

Once `lightrun.com/agent-type: "java"` is present, the webhook **denies admission of the pod**
(the pod is not created) in each of the following cases:

- A required annotation (`lightrun.com/agent-pool`, `lightrun.com/container-selector`,
  `lightrun.com/agent-env-var-name`) is missing.
- The named `AgentPool` doesn't exist in the pod's namespace, or its `spec.secretRef.name` is empty.
- `lightrun.com/container-selector` resolves to zero container names (e.g. it's set to an empty
  or all-whitespace string).
- One or more names in `lightrun.com/container-selector` don't match any container in the pod
  spec ("unable to find matching container to patch").
- `lightrun.com/agent-config` isn't valid JSON, or isn't a JSON object of strings.
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
> [`AgentPool`/Pod-annotation mechanism](#agentpool-crd-and-pod-annotations-current-recommended)
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
