# ── Build stage ───────────────────────────────────────────────────────────────
# For local Docker builds only. Run from the repo root (where go.mod lives):
#   docker build -f hello-world/Dockerfile -t hello-mcp .
#
# For Cloud Run deploys, use the Dockerfile at the repo root — gcloud run deploy
# requires the Dockerfile to be at the root of the --source directory.
FROM golang:1.25 AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full module (pkg/mcpauth + hello-world source).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/hello-mcp ./hello-world

# ── Runtime stage ─────────────────────────────────────────────────────────────
# Distroless static: no shell, minimal attack surface.
FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/hello-mcp /hello-mcp
# Cloud Run sets $PORT (default 8080); our server reads it.
EXPOSE 8080
ENTRYPOINT ["/hello-mcp"]
