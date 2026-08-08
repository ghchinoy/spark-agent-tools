# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/omi-bridge ./omi-bridge

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /bin/omi-bridge /omi-bridge

EXPOSE 8080
ENTRYPOINT ["/omi-bridge"]
