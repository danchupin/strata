# syntax=docker/dockerfile:1.7

# Stage 1: build the React console with pnpm + Vite. Output (web/dist)
# is copied into the Go build stage and embedded via go:embed.
FROM node:20-alpine AS web
RUN corepack disable && npm install -g pnpm@9
WORKDIR /web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build

FROM quay.io/ceph/ceph:v19.2.3 AS build

RUN printf '%s\n' \
    '[ceph-squid]' \
    'name=Ceph Squid ($basearch)' \
    'baseurl=https://download.ceph.com/rpm-squid/el9/$basearch/' \
    'gpgcheck=1' \
    'gpgkey=https://download.ceph.com/keys/release.asc' \
    'enabled=1' \
    > /etc/yum.repos.d/ceph.repo \
 && rpm --import https://download.ceph.com/keys/release.asc \
 && dnf install -y --setopt=install_weak_deps=False \
        gcc make git glibc-devel \
        librados-devel librbd-devel \
 && dnf clean all

ARG GOVERSION=1.25.1
RUN ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') \
 && curl -fsSL "https://go.dev/dl/go${GOVERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xzf - \
 && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
 && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

ENV CGO_ENABLED=1 PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -tags ceph -trimpath -o /out/strata ./cmd/strata

FROM quay.io/ceph/ceph:v19.2.3
ARG GIT_SHA=unknown
LABEL org.opencontainers.image.title="strata" \
      org.opencontainers.image.description="S3-compatible object gateway (single strata binary; admin subcommand consolidates the legacy strata-admin)" \
      org.opencontainers.image.source="https://github.com/danchupin/strata" \
      org.opencontainers.image.revision="${GIT_SHA}"
COPY --from=build /out/strata /usr/local/bin/strata
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/strata"]
CMD ["server"]
