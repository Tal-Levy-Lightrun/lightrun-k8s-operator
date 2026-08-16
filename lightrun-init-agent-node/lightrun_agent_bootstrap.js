'use strict';

// Loaded via `NODE_OPTIONS=--require <mountPath>/agent/lightrun_agent_bootstrap.js` (see
// internal/controller/patch_funcs_node.go, nodeAgentEnvVarArgument), before the target
// application's own code runs. A failure here must never crash the host application, so agent
// load/registration is wrapped defensively - the app must be able to start with or without Lightrun.

const path = require('path');

const agentDir = __dirname;

try {
  const lightrun = require(path.join(agentDir, 'node_modules', 'lightrun'));

  const config = {
    // agentConfigFile/metadata.filename default to being resolved relative to process.cwd() (the
    // app's own working directory, per src/agent/config.ts defaults), not to this bootstrap file's
    // directory - so they must be pointed at the mounted agent directory explicitly.
    agentConfigFile: path.join(agentDir, 'agent.config'),
    metadata: {filename: path.join(agentDir, 'agent.metadata.json')},
  };

  const cliFlags = process.env.LIGHTRUN_AGENT_CLI_FLAGS;
  if (cliFlags) {
    // Unlike Java's `-agentpath:...=cliFlags` (a single opaque string appended to JAVA_TOOL_OPTIONS),
    // the Node agent's start() takes a plain config object - so LIGHTRUN_AGENT_CLI_FLAGS is expected
    // to be a JSON-encoded partial config, e.g. '{"agentTags":["foo"],"lightrunWaitForInit":true}',
    // shallow-merged on top of the paths above.
    try {
      Object.assign(config, JSON.parse(cliFlags));
    } catch (parseErr) {
      console.error('[lightrun] Failed to parse LIGHTRUN_AGENT_CLI_FLAGS as JSON, ignoring:', parseErr.message);
    }
  }

  lightrun.start(config);
} catch (err) {
  console.error('[lightrun] Failed to load Node.js agent, continuing without instrumentation:', err);
}
