.PHONY: build run test test-integration clean docker docker-up docker-down lint fmt \
       integration-up integration-down

BINARY := funbot
MODULE := github.com/venatiodecorus/funbot
GOFLAGS := -trimpath
LDFLAGS := -s -w

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
	docker build -f deploy/docker/Dockerfile -t funbot:latest .

docker-up:
	docker compose -f deploy/docker-compose.yaml up --build

docker-down:
	docker compose -f deploy/docker-compose.yaml down

deps:
	go mod tidy
