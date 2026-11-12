# Multi-stage build for the sc-component-backup service.
#
# Shape cribbed from forge-notifier/Dockerfile:
#   1. Node builder — compile the React SPA into service/internal/ui/dist.
#   2. Go builder — build the Go binary with the SPA embedded via
#      //go:embed all:dist.
#   3. Lambda runtime — public.ecr.aws/lambda/provided:al2023 as the
#      final base; the binary runs under aws-lambda-go's lambda runtime
#      or as a standalone HTTP server when PORT is set (both shapes work
#      because go-aws-lambda-sdk's sdkservice detects the environment).
#
# Build args:
#   - VERSION    (auto-injected by SC per fleet CLAUDE.md "How SC builds images")
#   - COMMIT     (operator-declared under client.yaml image.build.args — see
#                 sc-auto-inject-github-sha-and-repository-build-args roadmap item)
#   - REVISION   (same)
#
# NO Dockerhub push from this repo's CI — see root CLAUDE.md for why. The
# `docker build` is run at smoke time (local or on the target VPS) with
# `docker load` of the resulting image.

ARG GOLANG_VERSION=1.26-alpine
ARG NODE_VERSION=20-alpine
ARG RUNTIME_BASE=public.ecr.aws/lambda/provided:al2023

# -----------------------------
# Stage 1 — Build the SPA
# -----------------------------
FROM node:${NODE_VERSION} AS ui-builder

WORKDIR /ui
COPY internal/ui/package.json ./
# COPY internal/ui/package-lock.json ./   ← uncomment once a lockfile exists
RUN npm install --no-audit --no-fund
COPY internal/ui/ ./
RUN npm run build:prod

# -----------------------------
# Stage 2 — Build the Go binary
# -----------------------------
FROM golang:${GOLANG_VERSION} AS go-builder

RUN apk add --no-cache ca-certificates git make

WORKDIR /src

# Copy module files first for layer-cache friendliness.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# Overwrite internal/ui/dist with the compiled SPA from stage 1.
COPY --from=ui-builder /ui/dist ./internal/ui/dist

ARG VERSION=dev
ARG COMMIT=dev
ARG REVISION=dev

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.revision=${REVISION}" \
      -o /out/backup ./cmd/backup

# -----------------------------
# Stage 3 — Runtime
# -----------------------------
FROM ${RUNTIME_BASE} AS runtime

LABEL org.opencontainers.image.title="SC Component Backup"
LABEL org.opencontainers.image.source="https://github.com/simple-container-com/sc-component-backup"
LABEL org.opencontainers.image.licenses="MIT"

# Russian CA carry-over from forge-notifier — placeholder directory, no
# certs yet. Real components talking to api.giga.chat / ngw.devices.sberbank.ru
# add the .crt files here + an update-ca-trust step.
COPY build/ca/ /etc/pki/ca-trust/source/anchors/

# The lambda base image's default WORKDIR is /var/task.
COPY --from=go-builder /out/backup /var/task/bootstrap

# For plain-HTTP mode (non-Lambda), run directly:
ENV PORT=8080
EXPOSE 8080

# Lambda runtime detects provided.al2023 and invokes /var/task/bootstrap.
# For VM/compose deploys, override with CMD ["/var/task/bootstrap"] in
# the deploy template's compose file.
