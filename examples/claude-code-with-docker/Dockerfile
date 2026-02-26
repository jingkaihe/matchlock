FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
  ca-certificates \
  curl \
  util-linux \
  systemd \
  systemd-sysv \
  dbus \
  iptables \
  iproute2 \
  dnsutils \
  git \
  && install -m 0755 -d /etc/apt/keyrings \
  && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
  && chmod a+r /etc/apt/keyrings/docker.asc \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null \
  && apt-get update \
  && apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
  && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
  && apt-get install -y nodejs \
  && npm i -g @anthropic-ai/claude-code@latest \
  && update-alternatives --set iptables /usr/sbin/iptables-legacy \
  && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy \
  && install -d -m 0755 /etc/docker \
  && printf '{\n  "storage-driver": "vfs",\n  "features": {\n    "containerd-snapshotter": false\n  }\n}\n' > /etc/docker/daemon.json \
  && useradd -m -s /bin/bash -G docker agent \
  && install -d -m 700 -o agent -g agent /home/agent/.claude \
  && install -d -m 755 -o agent -g agent /home/agent/workspace \
  && systemctl enable containerd.service docker.service \
  && touch /etc/machine-id \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/*

COPY --chown=agent:agent claude-settings.json /home/agent/.claude/settings.json

ENTRYPOINT ["unshare", "--pid", "--fork", "--mount-proc", "/usr/sbin/init"]
