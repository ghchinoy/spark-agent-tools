# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25 AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build a statically linked binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/hello-mcp .

# ── Runtime stage ───────────────────────────────────────────────────────────────
# Distroless static: no shell, minimal attack surface.
FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/hello-mcp /hello-mcp
# Cloud Run sets $PORT (default 8080); our server reads it.
EXPOSE 8080
ENTRYPOINT ["/hello-mcp"]
