# syntax=docker/dockerfile:1

# 与 CI 保持同步
ARG CLOUDFLARED_VERSION=2026.7.2

# ============================================================
# Stage 1: 构建前端 (React + Vite)
# 前端产物是纯静态文件，只需构建一次，与目标平台无关
# ============================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

ARG BUILD_VERSION=dev

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY frontend/ .
RUN VITE_APP_VERSION=${BUILD_VERSION} npm run build

# ============================================================
# Stage 2: 构建 Go 后端
# 使用 BUILDPLATFORM 原生运行 + TARGETARCH 交叉编译
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS go-builder

ARG TARGETARCH
ARG BUILD_VERSION=dev

# 国内构建走 goproxy.cn，避免直连 proxy.golang.org 断流（unexpected EOF）
ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=frontend-builder /frontend/dist ./frontend/dist

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/codex2api/internal/version.Version=${BUILD_VERSION}" -o /codex2api .

# ============================================================
# Stage 3: 最终运行镜像（可选内嵌 Cloudflare Tunnel）
# ============================================================
FROM alpine:3.19

ARG TARGETARCH
ARG CLOUDFLARED_VERSION

RUN apk add --no-cache ca-certificates curl su-exec tzdata && \
    addgroup -S -g 10001 codex2api && \
    adduser -S -D -H -u 10001 -G codex2api codex2api && \
    mkdir -p /data /app/logs /tmp && \
    chown -R codex2api:codex2api /data /app && \
    case "${TARGETARCH}" in \
      amd64) CF_ARCH=amd64 ;; \
      arm64) CF_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl -fsSL -o /usr/local/bin/cloudflared \
      "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${CF_ARCH}" && \
    chmod 0755 /usr/local/bin/cloudflared && \
    cloudflared --version && \
    apk del curl

COPY --from=go-builder --chmod=0755 /codex2api /usr/local/bin/codex2api
COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/codex2api-entrypoint

WORKDIR /app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/codex2api-entrypoint"]
CMD ["/usr/local/bin/codex2api"]
