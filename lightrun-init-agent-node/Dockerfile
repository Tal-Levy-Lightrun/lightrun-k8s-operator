ARG base_image_tag=alpine-3.24.1-r0

FROM lightruncom/prod-base:${base_image_tag}

# Placeholder: the real proprietary Lightrun Node.js agent artifact is an external build input not
# available in this repo (mirrors Java's $FILE build-arg unzip, which is out of scope here). This
# bakes in a minimal bootstrap script so the injection wiring (volume, init container, NODE_OPTIONS,
# pod Ready) can be validated end-to-end without the real product binary.
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
