# syntax=docker/dockerfile:1.7

FROM golang:1.26-bookworm AS builder

WORKDIR /src

ENV PATH=/usr/local/go/bin:${PATH} \
    CGO_ENABLED=0 \
    GOEXPERIMENT=jsonv2

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG GIT_SHA=dev
RUN go build \
      -buildvcs=false \
      -trimpath \
      -ldflags="-s -w -X main.gitSHA=${GIT_SHA}" \
      -o /out/kirocc \
      ./cmd/kirocc

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata busybox-extras \
    && addgroup -S kirocc \
    && adduser -S -D -H -G kirocc kirocc

WORKDIR /app
COPY --from=builder /out/kirocc /app/kirocc

USER kirocc:kirocc

ENV KIROCC_HOST=0.0.0.0 \
    KIROCC_ADMIN_HOST=0.0.0.0 \
    KIROCC_POOL_STRATEGY=least-inflight

EXPOSE 9326 3457

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -T 3 -O- http://127.0.0.1:9326/health >/dev/null || exit 1

ENTRYPOINT ["/app/kirocc"]
