FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    gh \
    sudo \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm i -g @anthropic-ai/claude-code@latest \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash agent \
    && usermod -aG sudo agent \
    && printf 'agent ALL=(ALL) NOPASSWD:ALL\n' >/etc/sudoers.d/agent \
    && chmod 0440 /etc/sudoers.d/agent \
    && mkdir -p /home/agent/workspace /workspace /home/agent/.claude \
    && chown -R agent:agent /home/agent /workspace

COPY --chmod=755 entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --chown=agent:agent claude-settings.json /home/agent/.claude/settings.json

USER agent
WORKDIR /home/agent
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
