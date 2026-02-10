# Stage 1: Build
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o conduit-expose .

# Stage 2: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /build/conduit-expose /usr/local/bin/conduit-expose

# GeoIP database (optional, downloaded at build time with MaxMind license key)
ARG MAXMIND_LICENSE_KEY
RUN if [ -n "$MAXMIND_LICENSE_KEY" ]; then \
      wget -qO /tmp/geo.tar.gz "https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-Country&license_key=${MAXMIND_LICENSE_KEY}&suffix=tar.gz" && \
      mkdir -p /data && \
      tar -xzf /tmp/geo.tar.gz -C /tmp && \
      mv /tmp/GeoLite2-Country_*/GeoLite2-Country.mmdb /data/ && \
      rm -rf /tmp/geo.tar.gz /tmp/GeoLite2-Country_*; \
    fi

EXPOSE 8081
ENTRYPOINT ["conduit-expose"]
