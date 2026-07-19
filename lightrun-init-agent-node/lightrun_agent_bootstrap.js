// Placeholder bootstrap: the real proprietary Lightrun Node.js agent is not available in this repo; this only proves the --require injection wiring works end-to-end.
console.log(
  '[lightrun] Node.js agent bootstrap loaded (placeholder). LIGHTRUN_SERVER=' +
    (process.env.LIGHTRUN_SERVER || '') +
    ' LIGHTRUN_AGENT_CLI_FLAGS=' +
    (process.env.LIGHTRUN_AGENT_CLI_FLAGS || '')
);
