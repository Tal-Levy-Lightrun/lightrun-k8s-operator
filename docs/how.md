### How it works

The operator currently supports two independent mechanisms for injecting the Lightrun Java agent.
Both can be enabled at the same time in the same operator install; enabling one does not disable
the other. See [custom_resource.md](custom_resource.md) for full field/annotation reference on
both.

## Pod-mutating webhook + `AgentPool` (current, recommended)

- The user creates an `AgentPool` CR (namespaced) in the same namespace as the Secret holding
  `lightrun_key`/`pinned_cert_hash`, and annotates a Pod (directly, or via a Deployment/StatefulSet's
  Pod template) with the `lightrun.com/*` annotations described in
  [custom_resource.md](custom_resource.md#agentpool-crd-and-pod-annotations-current-recommended) —
  including `lightrun.com/agent-pool` naming that `AgentPool`.
- This requires the operator's mutating admission webhook to be enabled
  (`webhook.enabled: true` in the Helm chart, or `--webhook-enabled` on the manager binary — see
  the [chart README](../charts/lightrun-operator/README.md) for the full `webhook.*` values,
  including the cert-manager prerequisite).
- The webhook intercepts every Pod `create` admission request cluster-wide. For each incoming Pod:
  - If `lightrun.com/agent-type` is absent, or set to a value the webhook doesn't recognize (only
    `"java"` is implemented today), the Pod is **admitted unchanged** — no mutation, no error.
  - If it's a recognized value, the webhook resolves the named `AgentPool` in the pod's own
    namespace (a live API read), validates the remaining required annotations, and mutates the
    incoming Pod object **in place, before it's persisted**:
    - Adds an `emptyDir` shared volume and a ConfigMap volume (agent config + metadata, built from
      the `lightrun.com/agent-config`/`lightrun.com/agent-tags`/`lightrun.com/agent-name`
      annotations). The ConfigMap is named from a hash of its own content, so pods with identical
      agent config naturally converge on and reuse the same ConfigMap instead of creating a new
      one per pod.
    - Optionally adds a Secret volume (when `lightrun.com/use-secrets-as-mounted-files` is true,
      the default) sourced from the resolved `AgentPool`'s `secretRef`.
    - Adds a `lightrun-installer` init container that stages the agent binary into the shared
      volume.
    - Mounts the shared volume into each container named by `lightrun.com/container-selector`, and
      appends the `-agentpath:...` argument to the env var named by
      `lightrun.com/agent-env-var-name` on those containers.
  - If any required annotation is missing, the named `AgentPool` can't be resolved, no container in
    `lightrun.com/container-selector` matches the pod spec, or the resulting env var value would
    exceed Java's 1024-character limit, the webhook **denies admission of that Pod** — see
    [custom_resource.md](custom_resource.md#misconfiguration--deny-behavior) for the exact list.
- **No reconcile loop, no workload patching, and no pod recreation-on-CR-deletion for this
  mechanism.** Mutation happens exactly once, synchronously, at the moment a Pod is admitted —
  there is nothing watching or re-applying it afterwards. Two important consequences:
  - **Existing, already-running pods are never retroactively mutated.** Enabling the webhook (or
    creating/updating an `AgentPool`, or adding annotations to a Deployment's Pod template) only
    affects **newly created** pods from that point on. To inject the agent into an already-running
    workload, you must cause its pods to be recreated (e.g. a rollout restart) after annotating it.
  - Deleting the `AgentPool` CR does **not** roll back or remove the agent from pods that already
    have it injected — there's no controller tracking that relationship. It only affects pods
    created afterward that still reference the (now-missing) pool name, which will fail admission.

## `LightrunJavaAgent` controller (legacy)

> Still fully functional and supported for existing users; being phased out in favor of the
> webhook/`AgentPool` mechanism above. New installations should prefer that mechanism instead.

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
