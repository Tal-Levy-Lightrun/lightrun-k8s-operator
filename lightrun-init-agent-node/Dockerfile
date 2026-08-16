ARG base_image_tag=alpine-3.24.1-r0
ARG node_builder_image_tag=20-alpine

# $FILE is a zip containing a single `npm pack` tarball - unlike Java's agent.zip, its runtime
# dependencies aren't bundled and must be resolved via npm. This builder stage does that with a
# real Node/npm toolchain; the final stage below copies over only the resolved /agent tree, so the
# shipped image stays on the same minimal base Java's init container uses.
FROM node:${node_builder_image_tag} AS agent-builder
ARG FILE

RUN apk add --no-cache unzip

COPY lightrun-init-agent-node/$FILE /tmp/$FILE

RUN unzip -o /tmp/$FILE -d /tmp/nodejs-agent-pkg && \
    mkdir -p /agent/node_modules/lightrun && \
    tar -xzf /tmp/nodejs-agent-pkg/*.tgz --strip-components=1 -C /agent/node_modules/lightrun && \
    rm -rf /tmp/$FILE /tmp/nodejs-agent-pkg && \
    cd /agent/node_modules/lightrun && \
    # No package-lock.json ships with the artifact, and one committed here would drift from future
    # releases' manifests - generate one fresh from whatever this release ships, then install from it.
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
