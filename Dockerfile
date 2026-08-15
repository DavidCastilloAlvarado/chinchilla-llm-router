# ---- build stage ---------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache dependency downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary, no CGO.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-router .

# ---- runtime stage --------------------------------------------------------
FROM alpine:3.20

# Working directory: the router looks for ./.env here at startup (values
# from .env take priority over injected -e environment variables).
WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 router \
    && adduser -D -u 10001 -G router router

USER router

COPY --from=build /out/llm-router /usr/local/bin/llm-router
COPY config.yaml /etc/llm-router/config.yaml

ENV LLM_ROUTER_CONFIG=/etc/llm-router/config.yaml

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz > /dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/llm-router"]
