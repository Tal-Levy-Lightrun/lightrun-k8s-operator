### How it works

The operator currently supports two independent mechanisms for injecting the Lightrun Java agent.
Both can be enabled at the same time in the same operator install; enabling one does not disable
the other. See [custom_resource.md](custom_resource.md) for full field/annotation reference on
both.

## Pod-mutating webhook + `AgentPool`/`ClusterAgentPool` (current, recommended)

### The minimal-annotation flow

The design goal is that most pods need **zero or one annotation**, not a flat set of required
annotations repeated on every workload:

- An admin creates one `AgentPool` (namespaced) or `ClusterAgentPool` (cluster-scoped, for
  sharing across namespaces) holding the shared config: credentials (`secretRef`), server
  hostname, and the defaults for everything that used to be a required per-pod annotation
  (`containerSelector`, `agentEnvVarName`, `agentCliFlags`, `agentTags`, `agentName`,
  `agentConfig`, `useSecretsAsMountedFiles`).
- The admin sets `lightrun.com/inject-java: "true"` on the **Namespace** object once. Every pod
  created in that namespace afterward — regardless of what Deployment/Helm chart created it —
  inherits that default and gets the agent injected using the `AgentPool`/`ClusterAgentPool`
  named `default`, with **no pod-level annotation at all**.
- A workload that needs something different from the namespace default needs at most a
  couple of annotations: `lightrun.com/inject-java: <other-pool-name>` to use a different pool,
  or one `lightrun.com/<field>` override annotation to change a single field (e.g. its own
  `agent-tags`) while everything else still comes from the resolved pool.

See [custom_resource.md](custom_resource.md#quick-start-the-zerominimal-annotation-path-recommended)
for the full walkthrough and [custom_resource.md](custom_resource.md#pod-annotation-contract) for
every annotation and its precedence.

### Pool resolution and per-field merge

This requires the operator's mutating admission webhook to be enabled
(`webhook.enabled: true` in the Helm chart, or `--webhook-enabled` on the manager binary — see
the [chart README](../charts/lightrun-operator/README.md) for the full `webhook.*` values). The
webhook intercepts every Pod `create` admission request cluster-wide. For each incoming Pod:

- It first decides **whether and how** to inject, purely from annotations/namespace defaults (see
  [custom_resource.md](custom_resource.md#trigger-annotation-lightruncominject-java) for the full
  precedence): the pod's own `lightrun.com/inject-java`, then its own legacy-style
  `lightrun.com/agent-type`+`lightrun.com/agent-pool` trigger, then the namespace's
  `lightrun.com/inject-java` default, then no injection. A pod's own explicit trigger — either
  style — always wins over a namespace default.
- If a trigger fires, it resolves the named pool (a live API read of the `AgentPool` in the pod's
  own namespace, or the `ClusterAgentPool` cluster-wide — gated on the pod's namespace being in
  that pool's `allowedNamespaces`), giving a set of shared-default field values.
- It then **merges the pod's own per-field override annotations on top of the pool's defaults,
  field by field** — not all-or-nothing. A pod that sets only `lightrun.com/agent-tags` gets its
  own tags with every other field (secret, hostname, container selector, env var, ...) still
  coming from the pool.
- With the fully-resolved configuration in hand, it mutates the incoming Pod object **in place,
  before it's persisted**:
  - Adds an `emptyDir` shared volume and a ConfigMap volume (agent config + metadata, built from
    the resolved `agentConfig`/`agentTags`/`agentName`). The ConfigMap is named from a hash of its
    own content, so pods with identical agent config naturally converge on and reuse the same
    ConfigMap instead of creating a new one per pod.
  - Optionally adds a Secret volume (when the resolved `useSecretsAsMountedFiles` is true, the
    default) sourced from the resolved pool's Secret.
  - Adds a `lightrun-installer` init container that stages the agent binary into the shared
    volume.
  - Mounts the shared volume into each container named by the resolved container selector, and
    appends the `-agentpath:...` argument to the env var named by the resolved
    `agentEnvVarName` on those containers.
- If the pool can't be resolved, the pod's namespace isn't permitted to use a resolved
  `ClusterAgentPool`, no container in the resolved selector matches the pod spec, or the
  resulting env var value would exceed Java's 1024-character limit, the webhook **denies
  admission of that Pod** — see
  [custom_resource.md](custom_resource.md#misconfiguration--deny-behavior) for the exact list.
- **No reconcile loop, no workload patching, and no pod recreation-on-CR-deletion for this
  mechanism.** Mutation happens exactly once, synchronously, at the moment a Pod is admitted —
  there is nothing watching or re-applying it afterwards. Two important consequences:
  - **Existing, already-running pods are never retroactively mutated.** Enabling the webhook (or
    creating/updating a pool, or adding/changing annotations) only affects **newly created** pods
    from that point on. To inject the agent into an already-running workload, you must cause its
    pods to be recreated (e.g. a rollout restart) after annotating it (or its namespace).
  - Deleting an `AgentPool`/`ClusterAgentPool` does **not** roll back or remove the agent from
    pods that already have it injected — there's no controller tracking that relationship. It
    only affects pods created afterward that still reference the (now-missing) pool name, which
    will fail admission.

### `ClusterAgentPool` credentials: mirrored, not shared

A core Kubernetes Secret volume/`secretKeyRef` can only ever reference a Secret in the same
namespace as the pod — there's no way to mount one directly across namespaces. So a
`ClusterAgentPool`'s credential Secret is **mirrored**, not cross-mounted: a dedicated controller
(separate from the webhook's hot admission path) watches every `ClusterAgentPool` and its source
Secret, and for each namespace listed in `spec.allowedNamespaces` it creates and keeps in sync a
copy of that Secret under a deterministic name in that namespace, owned by the `ClusterAgentPool`
for automatic garbage collection. The webhook only ever references this **local mirrored Secret**
when injecting into a pod — never the cross-namespace source directly.

This also means access is actually revocable, not just gated going forward: when a namespace is
removed from a `ClusterAgentPool`'s `allowedNamespaces`, the mirroring controller deletes the
now-stale mirrored Secret from that namespace on its next reconcile, cleaning up the
already-mirrored credential rather than leaving it behind. See
[custom_resource.md](custom_resource.md#how-the-secret-gets-to-the-pods-namespace) for the full
mechanics, including the ownership-collision-refusal safety check that stops the controller from
silently adopting/overwriting an unrelated Secret that happens to collide with its deterministic
mirror name.

## `LightrunJavaAgent` controller (legacy)

> Still fully functional and supported for existing users; being phased out in favor of the
> webhook/`AgentPool`/`ClusterAgentPool` mechanism above. New installations should prefer that
> mechanism instead.

 - The User begins by creating a custom resource of `Kind: LightrunJavaAgent`  
  [Example](../config/samples/agents_v1beta_lightrunjavaagent.yaml)  
  [Detailed explanation of CR fields](custom_resource.md)
 - The Controller receives all updates about all CRs (custom resources) of kind `LightrunJavaAgent` across all the cluster or specific namespaces
   (subject to how it's been installed). 
 Every event related to these CRs triggers the reconcile loop of the controller. You can find logic of this loop [here](reconcile_loop.excalidraw.png)  
 - When triggered, the controller performs several actions:
   - Check if it has access to the target resource (Deployment or StatefulSet)
   - Fetch data from the CR secret
   - Create config map with agent config from CR data
   - Patch the target resource (Deployment or StatefulSet):
     - insert init container
     - add volume
     - map that volume to the specified container
     - add/update specified ENV variable in order to let Java know where agent files are found (the mapped volume)
 - After the target resource is patched, k8s will `recreate all the pods` in the Deployment or StatefulSet. New Pods will be initialized with the Lightrun agent
 - If user deletes the `LightrunJavaAgent` CR, the Controller will roll back all the changes to the target resource. This will trigger `recreation of all pods` again
 - [High level diagram](resource_relations.excalidraw.png) of resources created/edited by the operator

Unlike the webhook mechanism above, this reconcile loop **does** watch for changes and **does**
retroactively apply/roll back the patch by mutating the live Deployment/StatefulSet (which in turn
causes Kubernetes to recreate its pods) — it is not a one-time, admission-time mutation.
