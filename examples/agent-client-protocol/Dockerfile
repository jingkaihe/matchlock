FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash agent \
    && mkdir -p /home/agent \
    && chown -R agent:agent /home/agent

USER agent

ENV PATH="/home/agent/.local/bin:$PATH"
RUN curl -sSL https://raw.githubusercontent.com/jingkaihe/kodelet/main/install.sh | bash \
    && kodelet setup

WORKDIR /workspace

ENTRYPOINT ["kodelet", "acp"]
