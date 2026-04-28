# syntax=docker/dockerfile:1.7

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
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -tags ceph -trimpath -o /out/strata-gateway ./cmd/strata-gateway \
 && go build -tags ceph -trimpath -o /out/strata-lifecycle ./cmd/strata-lifecycle \
 && go build -tags ceph -trimpath -o /out/strata ./cmd/strata

FROM quay.io/ceph/ceph:v19.2.3
COPY --from=build /out/strata-gateway /usr/local/bin/strata-gateway
COPY --from=build /out/strata-lifecycle /usr/local/bin/strata-lifecycle
COPY --from=build /out/strata /usr/local/bin/strata
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/strata-gateway"]
