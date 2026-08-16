'use strict';

// Loaded via `NODE_OPTIONS=--require <mountPath>/agent/lightrun_agent_bootstrap.js`, before the
// app's own code runs. A failure here must never crash the host application.

const path = require('path');

const agentDir = __dirname;

try {
  const lightrun = require(path.join(agentDir, 'node_modules', 'lightrun'));

  const config = {
    // These default to resolving relative to process.cwd() (the app's own working directory), not
    // this file's directory - point them at the mounted agent directory explicitly.
    agentConfigFile: path.join(agentDir, 'agent.config'),
    metadata: {filename: path.join(agentDir, 'agent.metadata.json')},
  };

  const cliFlags = process.env.LIGHTRUN_AGENT_CLI_FLAGS;
  if (cliFlags) {
    // Unlike Java's opaque cliFlags string, start() takes a config object, so this is expected to
    // be a JSON-encoded partial config, e.g. '{"lightrunWaitForInit":true}', merged onto the above.
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
