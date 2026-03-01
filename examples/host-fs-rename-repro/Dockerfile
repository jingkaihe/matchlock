FROM ubuntu:24.04
RUN apt-get update && apt-get install -y systemd systemd-sysv git && \
    apt-get clean && rm -rf /var/lib/apt/lists/* && \
    git config --global safe.directory '*'
ENTRYPOINT ["unshare", "--pid", "--fork", "--mount-proc", "/lib/systemd/systemd"]
