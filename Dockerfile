# syntax=docker/dockerfile:1

# --- build the TypeScript ---
# tsconfig.json has "types": ["node"], so @types/node is needed here too: no --omit=dev.
FROM node:24-alpine AS web
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY index.html ./
COPY src/ ./src/
RUN npm run build

# --- build the Go binary ---
#
# CGO_ENABLED=0 is mandatory, not a preference: the final image is distroless and has no C
# library, and `modernc.org/sqlite` is a pure-Go SQLite for exactly this reason. Without it the
# build tries to compile runtime/cgo and fails when no C compiler is present.
#
# -trimpath keeps the module cache paths out of the binary; -s -w drop the symbol table and DWARF
# data, which nothing in production needs.
#
# The go.sum check runs first so a dependency change that was not vendored fails in seconds instead
# of after a full compile.
FROM golang:1.26-alpine AS api
WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/newton ./cmd/api \
    && CGO_ENABLED=0 go vet ./...

# --- assemble the static payload ---
#
# nginx used to serve these; the Go binary serves them now, which is what makes one origin and
# therefore __Host- cookies possible. index.html's importmap points at planck.mjs and the zod tree,
# so they are copied next to dist/ under node_modules/ where the importmap expects them.
FROM alpine:3.21 AS public
WORKDIR /public
COPY --from=web /app/index.html ./
COPY --from=web /app/dist/ ./dist/
COPY --from=web /app/node_modules/planck/dist/planck.mjs ./node_modules/planck/dist/planck.mjs
COPY --from=web /app/node_modules/zod/ ./node_modules/zod/

# --- run ---
#
# distroless static + nonroot: no shell, no package manager, no libc to exploit. That also means the
# container healthcheck has to be the binary itself (-healthcheck), because there is no curl or wget.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=api /out/newton /app/newton
COPY --from=public /public/ /app/public/

ENV ADDR=:8080 \
    STATIC=/app/public \
    DB=/data/newton.db

# /data is a volume; the nonroot user (65532) must be able to create the database file there.
VOLUME ["/data"]
USER nonroot:nonroot
EXPOSE 8080
STOPSIGNAL SIGTERM

# Real liveness: the binary GETs its own /api/health, which runs `select 1` and reads the migration
# version. Swarm's update/rollback config depends on this being real.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/app/newton", "-healthcheck"]

ENTRYPOINT ["/app/newton"]
