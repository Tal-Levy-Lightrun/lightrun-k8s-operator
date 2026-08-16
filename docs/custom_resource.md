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
  # useSecretsAsMountedFiles determines whether to use secret values as environment variables (false) or as mounted files (true)
  # Default is false for backward compatibility
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

## LightrunNodeAgent

`LightrunNodeAgent` (shortName `lrna`) is the Node.js equivalent of `LightrunJavaAgent`. The spec shape is identical (`initContainer`, `workloadName`, `workloadType`, `secretName`, `serverHostname`, `agentConfig`, `agentCliFlags`, `agentTags`, `agentName`, `containerSelector`, `useSecretsAsMountedFiles`), with the following Node-specific differences:

```yaml
apiVersion: agents.lightrun.com/v1beta
kind: LightrunNodeAgent
metadata:
  name: example-node-cr
spec:
  # Init container with agent.
  initContainer:
    image: "lightruncom/lightrun-init-agent-node:latest"
    # imagePullPolicy of the init container. Can be one of: Always, IfNotPresent, or Never.
    imagePullPolicy: "IfNotPresent"
    # Volume name in case you have some convention in the names
    sharedVolumeName: lightrun-agent-init-node
    # Mount path where volume will be parked. Various distributions may have it's limitations.
    sharedVolumeMountPath: "/lightrun"
  # Name of the workload that you are going to patch.
  # Has to be in the same namespace
  workloadName: app
  # Type of the workload that you are going to patch.
  # Has to be one of `Deployment` or `StatefulSet`
  workloadType: Deployment
  # Name of the secret where agent will take `lightrun_key` and `pinned_cert_hash` from
  # Has to be in the same namespace
  secretName: lightrun-node-secrets
  # Hostname of the server. Will be different for on-prem ans single-tenant installations
  # For saas it will be app.lightrun.com
  serverHostname: <lightrun_server>
  # Env var that will be patched with `--require <sharedVolumeMountPath>/agent/lightrun_agent_bootstrap.js`.
  # The patch is appended idempotently - a pre-existing NODE_OPTIONS value is never overwritten.
  # Defaults to "NODE_OPTIONS"
  agentEnvVarName: NODE_OPTIONS
  # Agent config will override default configuration with provided values
  agentConfig:
    max_log_cpu_cost: "2"
  # DIVERGENCE from LightrunJavaAgent: agentCliFlags is NOT concatenated into agentEnvVarName
  # (Java uses `-agentpath:...=cliFlags` syntax). Instead it's set as a separate
  # LIGHTRUN_AGENT_CLI_FLAGS env var on the app container. Unlike Java's single opaque string, the
  # Node agent's start() takes a config object, so this is expected to be a JSON-encoded partial
  # config, e.g. '{"lightrunWaitForInit":true}', shallow-merged in by the bootstrap script.
  agentCliFlags: ""
  # Tags that agent will be using. You'll see them in the UI and in the IDE plugin as well
  agentTags:
    - operator
  # Agent name. If not provided, pod name will be used
  #agentName: "operator-test-agent"
  # List of container names inside the pod of the deployment
  # If container not mentioned here it will be not patched
  containerSelector:
    - app
  # useSecretsAsMountedFiles determines whether to use secret values as environment variables (false) or as mounted files (true)
  # Default is false for backward compatibility
  useSecretsAsMountedFiles: false
---
apiVersion: v1
metadata:
  name: lightrun-node-secrets
stringData:
  lightrun_key: <lightrun_key_from_ui>
  pinned_cert_hash: <pinned_cert_hash>
kind: Secret
type: Opaque
```

Note: in the `lightrun-agents` Helm chart, a `nodeAgents[]` entry that doesn't set `agentPoolCredentials.existingSecret` gets a generated secret (and `secretName`) defaulting to `{{ .name }}-node-secret` - deliberately distinct from `javaAgents[]`'s `{{ .name }}-secret` default, so a Java and Node agent entry sharing the same `.name` don't collide on the same Secret.

### TypeScript support

The Lightrun Node.js agent never executes or parses `.ts` files directly - it attaches to the
**compiled JavaScript** via the V8 Inspector protocol, and maps breakpoints on `.ts` lines to the
compiled `.js` positions purely through source maps. For TypeScript apps to work:

- `tsconfig.json` must have `"sourceMap": true` (see [TypeScript's `sourceMap` option](https://www.typescriptlang.org/tsconfig#sourceMap)).
- Both the compiled `.js` file **and its `.js.map`** must be present on disk, next to each other, in
  the running container. This is the most common failure mode: multi-stage Docker builds that ship
  only compiled output and intentionally strip `.map` files will break breakpoint placement.
- On-the-fly transpilation (`ts-node`, `tsx`) is **not supported** - the agent only discovers
  `*.js.map` files that already exist on disk, so apps run this way (no separate compile step, no
  on-disk map file) will fail to resolve `.ts` breakpoints.

Bundled output (e.g. webpack) with source maps is supported as well.

### Known Node.js limitations

- **ESM (`"type": "module"`, native `import`) is supported.** The Lightrun agent attaches via the V8
  Inspector protocol (`Debugger.scriptParsed`/`setBreakpointByUrl`), which fires for a script
  regardless of whether it was loaded as CommonJS or ESM, and Node's `--require` flag preloading a
  CommonJS bootstrap file is independently compatible with an ESM main module. This differs from
  require-hook-based auto-instrumentation (e.g. OpenTelemetry's, Datadog's), which does depend on
  CommonJS's `Module._load` and can't see ES modules. One cosmetic difference: the agent's startup
  log line ("Lightrun Debugger is attached to ...") reports `undefined` for the entry file path on
  an ESM main module (vs. the real path for CommonJS), since it relies on `require.main`, which
  doesn't exist for ESM entry points - this doesn't affect file scanning or breakpoint placement,
  which are directory-based, not `require.main`-based.
- `cluster.fork()` worker processes inherit `NODE_OPTIONS` from the parent automatically and are instrumented correctly. Manually-spawned `worker_threads`, however, do **not** inherit the parent's environment unless the application explicitly passes `env: process.env` when creating the worker - this is a Node.js platform behavior, not something the operator can patch around.
- The Lightrun Node.js agent requires **Node.js 18+** (`engines.node` in the agent's `package.json`).
- **Ambiguous filenames in monorepos/bundled output**: if two files share the same relative suffix, breakpoint placement can fail with a "more than one possible match" error. Use the agent's `appPathRelativeToRepository`/`pathResolver` config (via `agentConfig`) to disambiguate - relevant here since `containerSelector` targets one container/service at a time, which is exactly the shape where this can occur.
