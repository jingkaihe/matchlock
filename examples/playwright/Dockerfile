FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# System deps + Node.js 22 + Chromium runtime deps + non-root user
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates git \
    libnss3 libnss3-tools \
    libatk1.0-0t64 libatk-bridge2.0-0t64 libcups2t64 libdrm2 \
    libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
    libgbm1 libpango-1.0-0 libcairo2 libasound2t64 libatspi2.0-0t64 \
    libwayland-client0 fonts-liberation fonts-noto-color-emoji xdg-utils \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash agent

# Install Claude Code + Playwright MCP as agent user
USER agent
ENV NPM_CONFIG_PREFIX=/home/agent/.npm-global
ENV PATH="/home/agent/.npm-global/bin:$PATH"
RUN npm install -g @anthropic-ai/claude-code \
    && npm cache clean --force \
    && rm -rf /tmp/*

# Config files + entrypoint
USER root
COPY claude-settings.json /home/agent/browser-use/.claude/settings.json
COPY claude.json /home/agent/.claude.json
COPY mcp-config.json /home/agent/browser-use/.mcp.json
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN mkdir -p /home/agent/.claude /workspace \
    && chmod +x /usr/local/bin/entrypoint.sh \
    && chown -R agent:agent /home/agent/.claude /home/agent/.claude.json /home/agent/browser-use /workspace

USER agent
WORKDIR /home/agent/browser-use
