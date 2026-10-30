# Ubuntu 26.04 hosts run this Ubuntu 22.04 userspace to retain release compatibility.
FROM ubuntu:22.04@sha256:076e7bf82319619f28dd0aa9c02ab34b055ad37100395b4f49a2ffba04fb5e11
ARG DEBIAN_FRONTEND=noninteractive
ARG APT_MIRROR=http://archive.ubuntu.com/ubuntu
ARG NODE_VERSION
ARG NODE_SHA256
ARG RUSTUP_DIST_SERVER=https://static.rust-lang.org
ARG RUSTUP_UPDATE_ROOT=https://static.rust-lang.org/rustup
COPY extra-ca/ /usr/local/share/ca-certificates/
RUN sed -i "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR}|g; s|http://security.ubuntu.com/ubuntu|${APT_MIRROR}|g" /etc/apt/sources.list \
 && printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
 && chmod +x /usr/sbin/policy-rc.d \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git jq sudo python3 python3-pip tzdata iproute2 \
      build-essential pkg-config cmake lld clang libclang-dev file xz-utils unzip zip tini \
      libicu70 libkrb5-3 zlib1g libunwind8 liblttng-ust1 \
      libssl-dev libwebkit2gtk-4.1-dev libayatana-appindicator3-dev \
      libpipewire-0.3-dev libdrm-dev libgbm-dev libegl-dev meson ninja-build time \
      docker.io docker-compose-v2 docker-buildx gh \
 && update-ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY artifacts/runner.tar.gz /tmp/runner.tar.gz
RUN echo 'baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40  /tmp/runner.tar.gz' | sha256sum -c - \
 && mkdir -p /opt/actions-runner \
 && tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner \
 && rm /tmp/runner.tar.gz
COPY artifacts/node.tar.xz /tmp/node.tar.xz
RUN test -n "$NODE_VERSION" && test -n "$NODE_SHA256" \
 && echo "$NODE_SHA256  /tmp/node.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 \
 && rm /tmp/node.tar.xz \
 && node --version | grep -Fx "$NODE_VERSION"
COPY artifacts/pipewire.tar.gz /tmp/pipewire.tar.gz
RUN echo '9c45eef65e66224804ae8671849452a7f221e913813072b3aad346f20df666a8  /tmp/pipewire.tar.gz' | sha256sum -c -
COPY ci-libpipewire-build.sh /tmp/ci-libpipewire-build.sh
RUN PINVOU_PIPEWIRE_SOURCE_ARCHIVE=/tmp/pipewire.tar.gz bash /tmp/ci-libpipewire-build.sh \
 && rm /tmp/pipewire.tar.gz /tmp/ci-libpipewire-build.sh
RUN groupadd --gid 1008 github-runner \
 && useradd --uid 1008 --gid 1008 --create-home --shell /bin/bash github-runner \
 && printf 'github-runner ALL=(ALL:ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/ci-runner \
 && chmod 0440 /etc/sudoers.d/ci-runner \
 && mkdir -p /opt/rust \
 && chown github-runner:github-runner /opt/rust
COPY artifacts/rustup-init /tmp/rustup-init
RUN echo '5be191e8c70a5007074254cb74d78dbfdd25cd5a61c27de7b8e5410122ec24f4  /tmp/rustup-init' | sha256sum -c - \
 && chmod 0755 /tmp/rustup-init
USER github-runner
ENV CARGO_HOME=/opt/rust/cargo RUSTUP_HOME=/opt/rust/rustup
RUN /tmp/rustup-init -y --no-modify-path --profile minimal --default-toolchain stable --component rustfmt,clippy
USER root
RUN rm /tmp/rustup-init
COPY artifacts/google-linux-signing-key.pub /tmp/google-linux-signing-key.pub
RUN apt-get update && apt-get install -y --no-install-recommends gnupg \
 && gpg --batch --no-default-keyring --keyring /tmp/google-ci-keyring.gpg --import /tmp/google-linux-signing-key.pub \
 && gpg --batch --no-default-keyring --keyring /tmp/google-ci-keyring.gpg \
      --export EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796 > /usr/share/keyrings/google-chrome.gpg \
 && test -s /usr/share/keyrings/google-chrome.gpg \
 && echo 'deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main' > /etc/apt/sources.list.d/google-chrome.list \
 && apt-get update && apt-get install -y --no-install-recommends google-chrome-stable \
 && rm -rf /var/lib/apt/lists/* /tmp/google-linux-signing-key.pub /tmp/google-ci-keyring.gpg*
COPY entrypoint.sh /usr/local/bin/ci-runner-entrypoint
RUN chmod 0755 /usr/local/bin/ci-runner-entrypoint
USER github-runner
ENV PATH=/opt/rust/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/ci-runner-entrypoint"]
