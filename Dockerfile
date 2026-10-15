# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/opencode2api .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -u 10001 -M -s /usr/sbin/nologin app \
 && mkdir -p /app/data \
 && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/opencode2api /app/opencode2api
COPY config.example.yaml /app/config.example.yaml
USER app
EXPOSE 8318
ENTRYPOINT ["/app/opencode2api"]
CMD ["-config", "/app/config.yaml"]
