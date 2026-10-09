# syntax=docker/dockerfile:1
FROM golang:1.27-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ci ./cmd/ci

FROM golang:1.27-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS runtime
ARG LLAR_REF=36ba7c64a7ccbb67e37731e34d828360650c65f6
ARG LLCPPG_REF=c01757554a9fddd817a77369e347795c32955f9a
ARG LLGO_REF=b86d349178d610e6b15e95c2a5fdfcb130d09fa1
ENV LLGO_ROOT=/opt/llgo \
    PATH="/usr/local/bin:/usr/lib/llvm-22/bin:${PATH}"

# Use llcppg's own CI installer for its LLGo/LLVM toolchain, inside the image.
RUN git clone https://github.com/goplus/llcppg.git /opt/llcppg \
    && git -C /opt/llcppg checkout --detach "${LLCPPG_REF}"
RUN LLGO_SRC=/opt/llgo LLGO_BIN_DIR=/usr/local/bin LLVM_VERSION=22 \
    GO_VERSION="$(go env GOVERSION | sed 's/^go//')" LLGO_REF="${LLGO_REF}" \
    /opt/llcppg/.github/scripts/install-llgo.sh
RUN cd /opt/llcppg && llgo build -o /usr/local/bin/llcppg ./cmd/llcppg

RUN git clone https://github.com/xgo-dev/llar.git /opt/llar \
    && git -C /opt/llar checkout --detach "${LLAR_REF}" \
    && cd /opt/llar \
    && go build -ldflags="-checklinkname=0" -o /usr/local/bin/llar ./cmd/llar
RUN llgo version && llar --help && llcppg -h

RUN apt-get update \
    && apt-get install -y --no-install-recommends redis-server redis-tools \
    && mkdir -p /var/lib/redis

COPY --from=build /out/ci /usr/local/bin/ci
COPY --chmod=755 docker/entrypoint.sh /usr/local/libexec/cibot-entrypoint
WORKDIR /app
# Public defaults let --env-file supply configuration without baking in secrets.
COPY .env.example .env
VOLUME ["/var/lib/redis"]
EXPOSE 80
ENTRYPOINT ["/usr/local/libexec/cibot-entrypoint"]
CMD ["serve"]
