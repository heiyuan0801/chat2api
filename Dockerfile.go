# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web-build
WORKDIR /src/web-vue
COPY web-vue/package.json web-vue/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY VERSION CHANGELOG.md /src/
COPY web-vue ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS go-build
WORKDIR /src/go-backend
COPY go-backend/go.mod go-backend/go.sum ./
RUN go mod download && go mod verify
COPY go-backend ./
COPY VERSION /src/VERSION
ARG TARGETOS
ARG TARGETARCH
ARG RELEASE_REPOSITORY=yukkcat/chatgpt2api
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.buildVersion=$(cat /src/VERSION) -X github.com/yukkcat/chatgpt2api/go-backend/internal/controlplane.goReleaseRepository=$RELEASE_REPOSITORY" -o /out/control-server ./cmd/control-server \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.buildVersion=$(cat /src/VERSION)" -o /out/upstream-probe ./cmd/upstream-probe

# Keep the PostgreSQL 18 client in the runtime for compatible pg_dump backups.
FROM postgres:18-bookworm AS app
LABEL org.opencontainers.image.title="ChatGPT2API Go" \
      io.chatgpt2api.runtime="go"
ENV TZ=Asia/Shanghai
WORKDIR /app
RUN mkdir -p /app/data /opt/chatgpt2api/web_dist \
 && chown postgres:postgres /app/data
# The PostgreSQL base removes its temporary ca-certificates dependency.
# Preserve Go's trusted HTTPS roots for account sync and upstream execution.
COPY --from=go-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=go-build /out/ /usr/local/bin/
COPY --from=web-build /src/web-vue/dist/ /opt/chatgpt2api/web_dist/
COPY VERSION /opt/chatgpt2api/VERSION
COPY --chmod=0755 deploy/go-entrypoint.sh /usr/local/bin/chatgpt2api-go-entrypoint
USER postgres
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=6s --start-period=20s --retries=6 \
 CMD ["control-server", "-healthcheck-url", "http://127.0.0.1:8080/api/settings"]
ENTRYPOINT ["chatgpt2api-go-entrypoint"]
CMD ["-background"]
