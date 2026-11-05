# Image for the remote-host integration test: OpenSSH server and client.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server openssh-client procps ca-certificates \
    && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd \
    && useradd -m -s /bin/bash algo && useradd -m -s /bin/bash hub
