.PHONY: build run test clean docker docker-up docker-down lint fmt

BINARY := funbot
MODULE := github.com/venatiodecorus/funbot
GOFLAGS := -trimpath
LDFLAGS := -s -w

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/funbot

run-controller:
	FUNBOT_ROLE=controller go run ./cmd/funbot

run-worker:
	FUNBOT_ROLE=worker go run ./cmd/funbot

test:
	go test ./... -v -race

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin/

docker:
	docker build -f deploy/docker/Dockerfile -t funbot:latest .

docker-up:
	docker compose -f deploy/docker-compose.yaml up --build

docker-down:
	docker compose -f deploy/docker-compose.yaml down

deps:
	go mod tidy
