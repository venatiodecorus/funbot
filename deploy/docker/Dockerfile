FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /funbot ./cmd/funbot

FROM gcr.io/distroless/static-debian12

COPY --from=builder /funbot /funbot

ENTRYPOINT ["/funbot"]
