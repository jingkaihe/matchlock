FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
  curl \
  ca-certificates \
  util-linux \
  systemd \
  systemd-sysv \
  dbus \
  iptables \
  iproute2 \
  dnsutils \
  && install -m 0755 -d /etc/apt/keyrings \
  && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
  && chmod a+r /etc/apt/keyrings/docker.asc \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null \
  && apt-get update \
  && apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
  && update-alternatives --set iptables /usr/sbin/iptables-legacy \
  && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy \
  && systemctl enable containerd.service docker.service \
  && touch /etc/machine-id \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/*

ENTRYPOINT ["unshare", "--pid", "--fork", "--mount-proc", "/sbin/init"]
