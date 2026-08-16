ARG base_image_tag=alpine-3.24.1-r0
ARG node_builder_image_tag=20-alpine

# The real Node.js agent release artifact ($FILE) is a zip containing a single `npm pack` tarball
# (lightrun-<version>.tgz) - unlike Java's agent.zip, it is not a flat, self-contained unzip: the
# package's runtime dependencies (axios, winston, source-map, etc. - see its package.json) are not
# bundled in the tarball and must be resolved via npm. This builder stage does that resolution with
# a real Node/npm toolchain, then the final stage below copies over only the resulting /agent tree,
# so the shipped image stays on the same minimal base Java's init container uses (no node/npm/apk
# left behind in the image that actually runs in the cluster).
FROM node:${node_builder_image_tag} AS agent-builder
ARG FILE

RUN apk add --no-cache unzip

COPY lightrun-init-agent-node/$FILE /tmp/$FILE

RUN unzip -o /tmp/$FILE -d /tmp/nodejs-agent-pkg && \
    mkdir -p /agent/node_modules/lightrun && \
    tar -xzf /tmp/nodejs-agent-pkg/*.tgz --strip-components=1 -C /agent/node_modules/lightrun && \
    rm -rf /tmp/$FILE /tmp/nodejs-agent-pkg && \
    cd /agent/node_modules/lightrun && \
    # The release artifact doesn't ship its own package-lock.json (npm pack doesn't include one), and
    # a lockfile hand-committed to this repo would drift out of sync with a future release's
    # package.json. Generate one fresh from whatever manifest this release actually ships, then
    # install strictly from it - deterministic for this build, without pinning across releases.
    npm install --package-lock-only --omit=dev && \
    npm ci --omit=dev && \
    npm cache clean --force

FROM lightruncom/prod-base:${base_image_tag}

COPY --from=agent-builder /agent /agent
COPY lightrun-init-agent-node/agent.config /agent/agent.config
COPY lightrun-init-agent-node/lightrun_agent_bootstrap.js /agent/lightrun_agent_bootstrap.js

RUN sed -i.bak "s|com.lightrun.secret=.*|com.lightrun.secret=|" /agent/agent.config && rm /agent/agent.config.bak && \
    sed -i.bak "s|pinned_certs=.*|pinned_certs=|" /agent/agent.config && rm /agent/agent.config.bak && \
    # In openshift UID will be dynamic per project, hence procide permissions to root group (defualt in k8s)
    chgrp -R 0 /agent && \
    chmod -R g=u /agent && \
    # Create secret directory
    mkdir -p /etc/lightrun/secret && \
    chgrp -R 0 /etc/lightrun/secret && \
    chmod -R g=u /etc/lightrun/secret

# Copy and set permissions for update_config.sh before switching user
COPY lightrun-init-agent-node/update_config.sh /update_config.sh
RUN chmod 750 /update_config.sh

USER 1000

CMD [ "/bin/sh", "/update_config.sh" ]
