.PHONY: build run test test-integration clean docker docker-up docker-down lint fmt \
       k8s-apply k8s-delete k8s-status integration-up integration-down

BINARY := funbot
MODULE := github.com/venatiodecorus/funbot
GOFLAGS := -trimpath
LDFLAGS := -s -w
K8S_DIR := deploy/k8s
NAMESPACE := funbot

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/funbot

run-controller:
	FUNBOT_ROLE=controller go run ./cmd/funbot

run-worker:
	FUNBOT_ROLE=worker go run ./cmd/funbot

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

# Kubernetes targets
k8s-apply:
	kubectl apply -f $(K8S_DIR)/namespace.yaml
	kubectl apply -f $(K8S_DIR)/rbac.yaml
	kubectl apply -f $(K8S_DIR)/configmap.yaml
	kubectl apply -f $(K8S_DIR)/proxy-secret.yaml
	kubectl apply -f $(K8S_DIR)/art-pvc.yaml
	kubectl apply -f $(K8S_DIR)/redis.yaml
	kubectl apply -f $(K8S_DIR)/art-cronjob.yaml
	kubectl apply -f $(K8S_DIR)/controller.yaml

k8s-delete:
	-kubectl delete -f $(K8S_DIR)/controller.yaml
	-kubectl delete -f $(K8S_DIR)/art-cronjob.yaml
	-kubectl delete -f $(K8S_DIR)/redis.yaml
	-kubectl delete -f $(K8S_DIR)/art-pvc.yaml
	-kubectl delete -f $(K8S_DIR)/proxy-secret.yaml
	-kubectl delete -f $(K8S_DIR)/configmap.yaml
	-kubectl delete -f $(K8S_DIR)/rbac.yaml
	-kubectl delete -f $(K8S_DIR)/namespace.yaml

k8s-status:
	@echo "=== Pods ==="
	@kubectl get pods -n $(NAMESPACE) 2>/dev/null || echo "Namespace $(NAMESPACE) not found"
	@echo "\n=== Deployments ==="
	@kubectl get deployments -n $(NAMESPACE) 2>/dev/null || true
	@echo "\n=== StatefulSets ==="
	@kubectl get statefulsets -n $(NAMESPACE) 2>/dev/null || true

# Create a worker deployment for a network. Usage: make k8s-add-worker NETWORK=efnet
k8s-add-worker:
ifndef NETWORK
	$(error NETWORK is required. Usage: make k8s-add-worker NETWORK=efnet)
endif
	@sed 's/NETWORK_NAME/$(NETWORK)/g' $(K8S_DIR)/worker-template.yaml | kubectl apply -f -
	@echo "Worker deployment created for network: $(NETWORK)"
