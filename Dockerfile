# Multi-stage build: frontend -> Go -> distroless.
FROM node:24-alpine AS web
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml web/tsconfig.base.json ./web/
COPY web/apps/staff/package.json ./web/apps/staff/
COPY web/packages ./web/packages
RUN cd web && pnpm install --frozen-lockfile --prod=false
COPY web ./web
ARG BRAND_PROFILE_DIGEST=default
RUN --mount=type=secret,id=brand_profile_bundle,required=false \
    set -eu; \
    if [ -f /run/secrets/brand_profile_bundle ]; then \
      mkdir -p /tmp/brand-profile; \
      tar -xf /run/secrets/brand_profile_bundle -C /tmp/brand-profile; \
      export BRAND_PROFILE_PATH=/tmp/brand-profile/profile.json; \
    fi; \
    echo "brand-profile=${BRAND_PROFILE_DIGEST}" >/dev/null; \
    # The backend's one brand value (mail signature, TOTP issuer) comes from the
    # same profile the admin pages are built from.
    node -e 'const fs=require("fs");const p=process.env.BRAND_PROFILE_PATH;const n=p?JSON.parse(fs.readFileSync(p,"utf8")).identity.name:"FairLB";if(!n)throw new Error("brand profile has no identity.name");fs.writeFileSync("/src/brand-name.txt",n)'; \
    cd web; \
    pnpm --fail-if-no-match --filter @fairlb/staff build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/apps/staff/dist ./web/apps/staff/dist
COPY --from=web /src/brand-name.txt /src/brand-name.txt
# `webembed` bakes the admin UI into the binary, so self-hosting means running
# one file rather than also configuring a static file server.
ARG VERSION=dev
# Without -X the binary reports "dev" forever, and `fairlb version` is the
# first thing anyone is asked for in a bug report.
# The brand name is quoted inside -ldflags (shell-like splitting): a name with a
# space would otherwise become a stray linker argument and fail the build.
RUN BRAND_NAME="$(cat /src/brand-name.txt)" && CGO_ENABLED=0 go build -tags webembed \
    -ldflags="-s -w -X main.version=${VERSION} -X 'github.com/fairlb/fairlb/foundation/brand.Name=${BRAND_NAME}'" \
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
