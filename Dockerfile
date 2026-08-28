# Multi-stage build: frontend -> Go -> distroless.
FROM node:24-alpine AS web
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml web/tsconfig.base.json ./web/
COPY web/apps/staff/package.json ./web/apps/staff/
COPY web/packages ./web/packages
RUN cd web && pnpm install --frozen-lockfile --prod=false
COPY web ./web
# No brand goes in here. The build carries the default profile as a complete
# brand of its own; a deployment mounts another one over it at runtime, via
# BRAND_PROFILE_DIR (ADR-0214).
RUN cd web && pnpm --fail-if-no-match --filter @fairlb/staff build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/apps/staff/dist ./web/apps/staff/dist
# `webembed` bakes the admin UI into the binary, so self-hosting means running
# one file rather than also configuring a static file server.
ARG VERSION=dev
# Without -X the binary reports "dev" forever, and `fairlb version` is the
# first thing anyone is asked for in a bug report.
RUN CGO_ENABLED=0 go build -tags webembed \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /fairlb ./cmd/fairlb

# The data directory has to exist in the image, owned by the user the process
# runs as. Docker seeds a fresh named volume from whatever is at that path in
# the image — including its ownership — so without this the volume arrives
# root-owned and the process cannot write the master key it just generated.
# Distroless has no shell, so the directory is made in the build stage and
# copied in with the right owner.
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=nonroot:nonroot /data /data
COPY --from=build /fairlb /fairlb
EXPOSE 8080
USER nonroot:nonroot
# The image has no shell and no curl, so the binary probes itself.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=12 \
    CMD ["/fairlb", "healthcheck"]
ENTRYPOINT ["/fairlb"]
CMD ["serve"]
