# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: build the frontend.
# ---------------------------------------------------------------------------
FROM node:22-alpine AS web-builder
WORKDIR /app/web

# The whole web tree in one COPY: .dockerignore already strips node_modules and
# the local dist, and a package.json-first layer cannot be used here because a
# COPY whose sources all miss is a hard error while the frontend is still being
# written in parallel.
COPY web/ ./

RUN set -eux; \
    if [ -f package.json ]; then \
        if [ -f package-lock.json ]; then npm ci; else npm install; fi; \
        npm run build; \
    else \
        # No frontend in the context yet: emit a shell so the Go embed step,
        # and therefore the whole image, still builds. Once web/package.json
        # exists this branch is never taken.
        echo 'WARNING: no web/package.json; emitting a placeholder dist' >&2; \
        mkdir -p dist; \
        printf '%s\n' '<!doctype html><meta charset="utf-8"><title>Pipeline Health</title><p>Frontend not built: web/package.json was absent at image build time. The API is live at /api/snapshot, /api/scenarios and /api/stream.' > dist/index.html; \
    fi; \
    # Fail loudly here rather than shipping an image whose UI is a 404.
    test -s dist/index.html

# ---------------------------------------------------------------------------
# Stage 2: build the server, embedding the frontend from stage 1.
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS go-builder
WORKDIR /src

# go.su[m] is an optional-match glob: the module has no external dependencies
# yet, so go.sum may legitimately not exist.
COPY go.mod go.su[m] ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/embed.go

# The freshly built UI, never the local one (.dockerignore excludes web/dist).
COPY --from=web-builder /app/web/dist ./web/dist

# CGO off so the binary is fully static and can run on distroless/static.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol table
# and DWARF, which is most of the size.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---------------------------------------------------------------------------
# Stage 3: runtime. Distroless static has no shell and no package manager, so
# the attack surface is the binary itself plus CA certificates.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=go-builder /out/server /server

# Cloud Run overrides this with its own PORT; the default keeps `docker run`
# and local testing working unchanged.
ENV PORT=8080
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/server"]
