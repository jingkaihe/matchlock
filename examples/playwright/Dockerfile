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
    && useradd -m -s /bin/bash agent \
    && mkdir -p /home/agent/.kodelet /home/agent/.local/bin /workspace \
    && chown -R agent:agent /home/agent /workspace

# Install kodelet + toad + bootstrap as agent user
USER agent
ENV PATH="/home/agent/.local/bin:$PATH"
RUN curl -sSL https://raw.githubusercontent.com/jingkaihe/kodelet/main/install.sh | bash \
    && curl -fsSL batrachian.ai/install | sh \
    && kodelet setup

# Config files + entrypoint (after install layers for better caching)
USER root
COPY --chown=agent:agent kodelet-config.yaml /home/agent/browser-use/kodelet-config.yaml
COPY --chmod=755 entrypoint.sh /usr/local/bin/entrypoint.sh

USER agent
WORKDIR /home/agent/browser-use
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
