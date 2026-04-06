FROM golang:1.24-alpine AS builder

ARG VERSION=dev
ARG COMMIT=none

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /funbot ./cmd/funbot

FROM gcr.io/distroless/static-debian12

LABEL org.opencontainers.image.source="https://github.com/venatiodecorus/funbot"
LABEL org.opencontainers.image.description="IRC bot with proxy-backed multi-client connections"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=builder /funbot /funbot

ENTRYPOINT ["/funbot"]
