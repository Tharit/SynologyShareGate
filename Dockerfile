FROM golang:1.24-alpine AS builder

WORKDIR /app

# Download dependencies first (layer cached unless go.mod/go.sum change).
COPY go.mod go.sum* ./
RUN go mod download

# Build a fully static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o syno-proxy .

# -------------------------
FROM scratch

# TLS CA certificates for HTTPS calls to the Synology NAS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /app/syno-proxy /syno-proxy

# Run as a non-root user (UID 10001 has no corresponding /etc/passwd entry in scratch,
# but the kernel enforces the numeric UID).
USER 10001

EXPOSE 8080

ENTRYPOINT ["/syno-proxy"]
