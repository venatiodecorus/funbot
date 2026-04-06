.PHONY: build run test test-integration clean docker docker-up docker-down lint fmt \
       integration-up integration-down

BINARY := funbot
MODULE := github.com/venatiodecorus/funbot
GOFLAGS := -trimpath
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/funbot

run:
	go run ./cmd/funbot

test:
	go test ./... -v -race

test-integration: integration-up
	@echo "Waiting for services to be ready..."
	@sleep 3
	go test -tags integration -v -timeout 120s ./test/integration/
	$(MAKE) integration-down

integration-up:
	docker compose -f test/integration/docker-compose.yaml up -d --wait

integration-down:
	docker compose -f test/integration/docker-compose.yaml down

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin/

docker:
	docker build -f deploy/docker/Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t funbot:$(VERSION) \
		-t funbot:latest .

docker-up:
	docker compose -f deploy/docker-compose.yaml up -d

docker-down:
	docker compose -f deploy/docker-compose.yaml down

docker-pull:
	docker compose -f deploy/docker-compose.yaml pull

deps:
	go mod tidy
