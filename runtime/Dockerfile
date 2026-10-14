# syntax=docker/dockerfile:1
#
# Reproducible Linux dev/workspace environment for JunKloud CKAD training.
#
# Two build targets:
#   workspace - everything a trainee needs: bash, kubectl, Helm, yq, kind,
#               the Docker CLI (talks to the host engine over a mounted
#               socket), and basic editors/HTTP tools.
#   dev       - workspace plus the Go compiler and the C toolchain the race
#               detector needs, for building/testing the Go CLI itself.
#
# Every downloaded tool is pinned to an exact version and verified against a
# SHA-256 checksum recorded in /versions.lock.yaml (see that file for
# provenance). `latest` is never used anywhere in this file.
FROM debian@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 AS base

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/home/learner

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        iproute2 \
        less \
        make \
        nano \
        procps \
        tar \
    && rm -rf /var/lib/apt/lists/*

# ---- pinned tool versions; must match /versions.lock.yaml ----
ARG KIND_VERSION=v0.33.0
ARG KIND_SHA256_AMD64=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
ARG KIND_SHA256_ARM64=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
ARG KUBECTL_VERSION=v1.37.0
ARG KUBECTL_SHA256_AMD64=6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f
ARG KUBECTL_SHA256_ARM64=922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3
ARG HELM_VERSION=v4.3.0
ARG HELM_SHA256_AMD64=86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb
ARG HELM_SHA256_ARM64=31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08
ARG YQ_VERSION=v4.54.1
ARG YQ_SHA256_AMD64=8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f
ARG YQ_SHA256_ARM64=189088da0c6429ec5178dfaab1a114805f6cab0b61b165ab236efedf1d57a71b
ARG DOCKER_CLI_VERSION=29.9.0
ARG DOCKER_CLI_SHA256_AMD64=33e1ab8b63d14bca449f7a3d30d7f6aa669daa544e60a86b17fec87f61afe2b4
ARG DOCKER_CLI_SHA256_ARM64=811f6eee271678fc98dbe5c4d896c00fc5fe2daa4f7a37069c17abc036eab85b
ARG COMPOSE_VERSION=v5.6.0
ARG COMPOSE_SHA256_AMD64=40343e21ca777173e69cff5dbafeb37c6f81f3b0d57d9e597f036e95eb63e76a
ARG COMPOSE_SHA256_ARM64=733ec76717ceb59052a9609b9dadfb523b2df8eab57a54212872d10a58078ea2
ARG BUILDX_VERSION=v0.38.0
ARG BUILDX_SHA256_AMD64=4fe4cc38adf48169132749b6ca22a990928db0118e3407584ee553723115d287
ARG BUILDX_SHA256_ARM64=38e890e1a162bfdbf32fbe91404991dc4fc28e55b0de901b34374a061bab4d2c

# kind: upstream Kubernetes-in-Docker node runner.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${KIND_SHA256_AMD64}" ;; \
        arm64) sha="${KIND_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/kind \
        "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${TARGETARCH}"; \
    echo "${sha}  /usr/local/bin/kind" | sha256sum -c -; \
    chmod +x /usr/local/bin/kind

# kubectl: pinned to the Kubernetes version kind's node image defaults to.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${KUBECTL_SHA256_AMD64}" ;; \
        arm64) sha="${KUBECTL_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/kubectl \
        "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    echo "${sha}  /usr/local/bin/kubectl" | sha256sum -c -; \
    chmod +x /usr/local/bin/kubectl

# Helm: tarball contains a linux-<arch>/helm nested path.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${HELM_SHA256_AMD64}" ;; \
        arm64) sha="${HELM_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/helm.tgz \
        "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    echo "${sha}  /tmp/helm.tgz" | sha256sum -c -; \
    tar -xzf /tmp/helm.tgz -C /tmp "linux-${TARGETARCH}/helm"; \
    install -m 0755 "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm; \
    rm -rf /tmp/helm.tgz "/tmp/linux-${TARGETARCH}"

# yq: YAML processor used to read versions.lock.yaml and problem schemas.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${YQ_SHA256_AMD64}" ;; \
        arm64) sha="${YQ_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/yq \
        "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${TARGETARCH}"; \
    echo "${sha}  /usr/local/bin/yq" | sha256sum -c -; \
    chmod +x /usr/local/bin/yq

# Docker CLI (client only, no daemon): talks to the host engine over the
# socket compose mounts in. download.docker.com publishes no standalone
# checksum file for these static tarballs, so this sha256 is self-computed
# from the artifact (see versions.lock.yaml for provenance).
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${DOCKER_CLI_SHA256_AMD64}"; dlarch=x86_64 ;; \
        arm64) sha="${DOCKER_CLI_SHA256_ARM64}"; dlarch=aarch64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/docker.tgz \
        "https://download.docker.com/linux/static/stable/${dlarch}/docker-${DOCKER_CLI_VERSION}.tgz"; \
    echo "${sha}  /tmp/docker.tgz" | sha256sum -c -; \
    tar -xzf /tmp/docker.tgz -C /tmp docker/docker; \
    install -m 0755 /tmp/docker/docker /usr/local/bin/docker; \
    rm -rf /tmp/docker.tgz /tmp/docker

# Docker Compose CLI plugin: the workspace/dev containers mount the host's
# Docker socket, so the runtime test (tests/runtime/workspace_test.sh) can
# drive `docker compose` itself from inside the container via that same
# socket (Docker-outside-of-Docker), exactly as `make test-runtime` does.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${COMPOSE_SHA256_AMD64}"; dlarch=x86_64 ;; \
        arm64) sha="${COMPOSE_SHA256_ARM64}"; dlarch=aarch64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose \
        "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${dlarch}"; \
    echo "${sha}  /usr/local/lib/docker/cli-plugins/docker-compose" | sha256sum -c -; \
    chmod +x /usr/local/lib/docker/cli-plugins/docker-compose

# Docker Buildx CLI plugin: `docker compose build` needs a buildx builder
# registered to build via BuildKit (which is what populates ARG TARGETARCH
# below); without it, compose silently falls back to the legacy builder and
# every per-arch RUN step here breaks.
RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${BUILDX_SHA256_AMD64}" ;; \
        arm64) sha="${BUILDX_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-buildx \
        "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-${TARGETARCH}"; \
    echo "${sha}  /usr/local/lib/docker/cli-plugins/docker-buildx" | sha256sum -c -; \
    chmod +x /usr/local/lib/docker/cli-plugins/docker-buildx

# learner user + a baked-in home skeleton. When compose mounts a fresh named
# volume over /home/learner, Docker seeds it from this image content on
# first use (and never again) -- that is how a user's dotfiles survive
# `docker compose down` + `up` across the one path that matters: an *empty*
# volume only ever gets this seed once. runtime/entrypoint.sh additionally
# guards against the (already empty) volume missing these files for any
# reason, without ever clobbering a file that is already there.
RUN groupadd -g 1000 learner \
    && useradd -m -u 1000 -g 1000 -d /home/learner -s /bin/bash learner \
    && { \
        echo '# JunKloud CKAD workspace shell'; \
        echo 'export PS1="[ckad] \u@\h:\w\$ "'; \
        echo 'if [ -f ~/.bash_aliases ]; then . ~/.bash_aliases; fi'; \
    } > /home/learner/.bashrc \
    && echo '# user aliases persist here across problem resets' > /home/learner/.bash_aliases \
    && mkdir -p /etc/skel \
    && cp /home/learner/.bashrc /etc/skel/.bashrc \
    && cp /home/learner/.bash_aliases /etc/skel/.bash_aliases \
    && chown -R learner:learner /home/learner

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /src
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["sleep", "infinity"]

# ---------------------------------------------------------------------------
FROM base AS workspace
USER learner

# ---------------------------------------------------------------------------
FROM base AS dev

ARG GO_VERSION=1.27.2
ARG GO_SHA256_AMD64=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
ARG GO_SHA256_ARM64=94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8

# gcc/libc6-dev: cgo, required for `go test -race`.
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "${TARGETARCH}" in \
        amd64) sha="${GO_SHA256_AMD64}" ;; \
        arm64) sha="${GO_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/go.tgz \
        "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    echo "${sha}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz

ENV PATH="/usr/local/go/bin:${PATH}" \
    GOPATH=/home/learner/go

USER learner
