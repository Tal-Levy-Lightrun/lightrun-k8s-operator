# [Lightrun k8s operator](https://github.com/lightrun-platform/lightrun-k8s-operator)

![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)

## Operator docs
[Github readme](https://github.com/lightrun-platform/lightrun-k8s-operator/tree/main/docs)

## Requirements

Kubernetes: `>= 1.19.0`

## Dependencies

Custom Resource of the operator is strictly depends on the secret with `lightrun_key` and `pinned_cert_hash` values  
[Example](https://github.com/lightrun-platform/lightrun-k8s-operator/tree/main/examples/lightrunjavaagent.yaml#L56)

The pod-mutating webhook (`webhook.enabled: true`) has **zero external prerequisites**: its TLS
serving certificate is fully self-managed at runtime by
[`open-policy-agent/cert-controller`](https://github.com/open-policy-agent/cert-controller) (the
same library [Gatekeeper](https://github.com/open-policy-agent/gatekeeper) uses) — it mints a
self-signed CA, rotates the serving cert, and keeps the `MutatingWebhookConfiguration`'s
`caBundle` patched, all as a normal controller running alongside the rest of the operator.
cert-manager (or any other external cert bootstrap) is not required, does not need to be
installed, and this chart no longer creates or references one.

## Pod-mutating webhook

Setting `webhook.enabled: true` turns on the mutating admission webhook that injects the Lightrun
Java agent into any pod carrying `lightrun.com/*` annotations (or inheriting a namespace-level
default — see below) and referencing an `AgentPool` (namespaced) or `ClusterAgentPool`
(cluster-scoped, for sharing across namespaces) CR — see the
[operator docs](https://github.com/lightrun-platform/lightrun-k8s-operator/tree/main/docs/custom_resource.md)
for the full annotation contract, and [how.md](https://github.com/lightrun-platform/lightrun-k8s-operator/tree/main/docs/how.md)
for how this compares to the legacy `LightrunJavaAgent` CR/controller mechanism, which keeps
working unchanged whether or not the webhook is enabled. The webhook is disabled by default so
existing installs are unaffected by upgrading.

The recommended pattern needs **zero or one annotation per pod**: create one `AgentPool`/
`ClusterAgentPool` named `default` and set `lightrun.com/inject-java: "true"` on the Namespace
object once — every pod created in that namespace afterward inherits it automatically. See the
[quick start](https://github.com/lightrun-platform/lightrun-k8s-operator/tree/main/docs/custom_resource.md#quick-start-the-zerominimal-annotation-path-recommended)
for the full walkthrough.

### `namespacedScope` and the webhook/`ClusterAgentPool` RBAC

`managerConfig.operatorScope.namespacedScope: true` scopes the operator's `Deployment`/
`StatefulSet`/`LightrunJavaAgent`/`AgentPool` RBAC down to `operatorScope.namespaces`. It does
**not** scope down anything cluster-inherently-scoped: `Namespace` reads (for the webhook's
namespace-level default lookup), `MutatingWebhookConfiguration` updates (for cert rotation),
`ClusterAgentPool` access, and `Secret` access (needed cluster-wide both by the
`ClusterAgentPool` secret-mirroring controller, which can mirror into any `allowedNamespaces`
regardless of the watch-namespace list, and by the cert-rotation controller, which must always
reach its own cert Secret in the operator's own namespace) are **always granted cluster-wide**,
by design, regardless of `namespacedScope` — these are inherently cluster-wide operations that
can't be meaningfully namespace-scoped.

## Installation  
- Add the repo to your Helm repository list
```sh 
helm repo add lightrun-k8s-operator https://lightrun-platform.github.io/lightrun-k8s-operator
```

-  Install the Helm chart:   
> _Using default [values](../lightrun-operator/values.yaml)_  
  
```sh
helm install lightrun-k8s-operator/lightrun-k8s-operator  -n lightrun-operator --create-namespace
```  

  > _Using custom values file_

```sh
helm install lightrun-k8s-operator/lightrun-k8s-operator  -f <values file>  -n lightrun-operator --create-namespace
```
> `helm upgrade --install` or `helm install --dry-run` may not work properly due to limitations of how Helm work with CRDs.
You can find more info [here](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/)


## Uninstall
```sh
helm delete lightrun-k8s-operator
```
> `CRDs` will not be deleted due to Helm CRDs limitations. You can learn more about the limitations [here](https://helm.sh/docs/topics/charts/#limitations-on-crds).

## Chart version vs controller version
For the sake of simplicity, we are keeping the convention of the same version for both the controller image and the Helm chart. This helps to ensure that controller actions are aligned with CRDs preventing failed resource validation errors.


## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| controllerManager.manager.image.repository | string | `"lightruncom/lightrun-k8s-operator"` |  |
| controllerManager.manager.image.tag | string | `"latest"` | For simplicity of version compatibilities we are keeping the same controller and chart versions So the most safe approach is to use same version as the Chart. When installing chart from the helm repo, every helm package version will have controller image set to chart version |
| controllerManager.manager.nodeSelector | object | `{}` |  |
| controllerManager.manager.resources.limits.cpu | string | `"500m"` |  |
| controllerManager.manager.resources.limits.memory | string | `"128Mi"` |  |
| controllerManager.manager.resources.requests.cpu | string | `"10m"` |  |
| controllerManager.manager.resources.requests.memory | string | `"64Mi"` |  |
| controllerManager.manager.tolerations | list | `[]` |  |
| controllerManager.replicas | int | `1` |  |
| managerConfig.healthProbe.bindAddress | string | `":8081"` |  |
| managerConfig.logLevel | string | `"info"` | Log level: 1 - 5 Higher number - more logs Documentation of logr module https://pkg.go.dev/github.com/go-logr/logr@v1.2.0#hdr-Verbosity On level info (0) (default) you'll see only deployments that are being added or deleted and errors On level 1 you'll see 1 additional log per every successful reconciliation loop run On level 2 you'll see all debug prints with intermediate steps while patching deployment per every reconciliation loop run |
| managerConfig.metrics.bindAddress | string | `":8080"` |  |
| managerConfig.operatorScope | object | `{"namespacedScope":false,"namespaces":["default"]}` | Operator may work in 2 scopes: cluster and namespaced Cluster scope will give permissions to operator to watch and patch deployment in the whole cluster With namespaced scope you need to provide list of namespaces that operator will be able to watch. Namespaced scope implemented by both controller code and creation of the appropriate Roles by the chart Any change to the list of namespaces will cause restart of the operator controller pod. |
| managerConfig.profiler.bindAddress | string | `""` |  |
| metricsService | object | `{"ports":[{"name":"http","port":8080,"protocol":"TCP","targetPort":8080}],"type":"ClusterIP"}` | Metrics service for prometheus compatible poller |
| nameOverride | string | `"lightrun-k8s-operator"` |  |
| webhook | object | `{"certDir":"/tmp/k8s-webhook-server/serving-certs","enabled":false,"failurePolicy":"Ignore","injection":{"initContainerImage":{"pullPolicy":"","repository":"lightruncom/k8s-operator-init-java-agent-linux","tag":"latest"},"sharedVolumeMountPath":"/lightrun","sharedVolumeName":"lightrun-agent"},"port":9443}` | Pod-mutating admission webhook that injects the Lightrun Java agent into annotated Pods, replacing the CR/controller-based patching mechanism. Additive today: the old CRD/controller keep running side by side until the webhook path is validated end-to-end. The webhook's TLS serving cert is fully self-managed by the operator at runtime (open-policy-agent/cert-controller) — no cert-manager or other external prerequisite needed. |
| webhook.certDir | string | `"/tmp/k8s-webhook-server/serving-certs"` | Directory the webhook server reads its TLS serving cert (tls.crt/tls.key) from; also where the self-managed CertRotator writes it. |
| webhook.enabled | bool | `false` | Set to true to enable the mutating webhook, its Service, its self-managed TLS cert Secret, and its cert-rotation controller. There is exactly one TLS bootstrap mode — no cert-manager toggle, no manual cert/caBundle escape hatch. |
| webhook.failurePolicy | string | `"Ignore"` | Admission FailurePolicy for the Pod mutating webhook. "Ignore" means webhook unavailability must not block Pod scheduling; "Fail" would block all Pod creates cluster-wide if the webhook is down, which is generally too risky for a cluster-wide Pod webhook. |
| webhook.injection | object | `{"initContainerImage":{"pullPolicy":"","repository":"lightruncom/k8s-operator-init-java-agent-linux","tag":"latest"},"sharedVolumeMountPath":"/lightrun","sharedVolumeName":"lightrun-agent"}` | Defaults for the Java agent injection mechanics; mirrors the old LightrunJavaAgent CR's initContainer/sharedVolume fields, now operator-wide instead of per-CR. |
| webhook.injection.initContainerImage.pullPolicy | string | `""` | Empty uses the cluster default image pull policy. |
| webhook.injection.initContainerImage.repository | string | `"lightruncom/k8s-operator-init-java-agent-linux"` |  |
| webhook.injection.initContainerImage.tag | string | `"latest"` |  |
| webhook.injection.sharedVolumeMountPath | string | `"/lightrun"` |  |
| webhook.injection.sharedVolumeName | string | `"lightrun-agent"` |  |
| webhook.port | int | `9443` | Port the webhook server listens on inside the manager container. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
