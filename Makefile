# ==============================================================================
# VARIABLES
# ==============================================================================
CLUSTER_NAME := antrea-poormans-pcap
IMAGE_NAME   := antrea-pcap:latest
NAMESPACE    := kube-system
TEST_NS      := test

# Tools
KUBECTL := kubectl
KIND    := kind
DOCKER  := docker
HELM    := helm
GO      := go

# ==============================================================================
# HELP / DEFAULT
# ==============================================================================
.PHONY: help
help: ## Display this help message
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.DEFAULT_GOAL := help

# ==============================================================================
# CHECKS & PRE-REQS
# ==============================================================================
.PHONY: check-tools
check-tools: ## Check if required tools are installed
	@command -v $(DOCKER) >/dev/null 2>&1 || { echo >&2 "Docker is required but not installed. Aborting."; exit 1; }
	@command -v $(KIND) >/dev/null 2>&1 || { echo >&2 "Kind is required but not installed. Aborting."; exit 1; }
	@command -v $(KUBECTL) >/dev/null 2>&1 || { echo >&2 "Kubectl is required but not installed. Aborting."; exit 1; }
	@command -v $(HELM) >/dev/null 2>&1 || { echo >&2 "Helm is required but not installed. Aborting."; exit 1; }
	@echo "All tools verified."

# ==============================================================================
# INFRASTRUCTURE (Kind + Antrea)
# ==============================================================================
.PHONY: cluster-up
cluster-up: check-tools ## Create Kind cluster and install Antrea
	@echo "Creating Kind cluster '$(CLUSTER_NAME)'..."
	@$(KIND) get clusters | grep -q $(CLUSTER_NAME) || $(KIND) create cluster --name $(CLUSTER_NAME) --config kind-config.yaml
	@echo "Installing Antrea CNI..."
	@$(HELM) repo add antrea https://charts.antrea.io
	@$(HELM) repo update
	@$(HELM) list -n $(NAMESPACE) | grep -q antrea || $(HELM) install antrea antrea/antrea -n $(NAMESPACE) --wait

.PHONY: cluster-down
cluster-down: ## Delete the Kind cluster
	@echo "Deleting cluster '$(CLUSTER_NAME)'..."
	@$(KIND) delete cluster --name $(CLUSTER_NAME)
	@rm -rf /tmp/pcap-captures

# ==============================================================================
# DEVELOPMENT (Build & Deploy)
# ==============================================================================
.PHONY: fmt
fmt: ## Run go fmt against code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	$(GO) vet ./...

.PHONY: build
build: ## Build Docker image
	@echo "Building Docker image '$(IMAGE_NAME)'..."
	$(DOCKER) build -t $(IMAGE_NAME) .

.PHONY: load
load: build ## Load Docker image into Kind
	@echo "Loading image into cluster..."
	$(KIND) load docker-image $(IMAGE_NAME) --name $(CLUSTER_NAME)

.PHONY: deploy
deploy: load ## Deploy Controller manifests to cluster
	@echo "Deploying manifests..."
	# This wildcard works even if you named it rabc.yaml or rbac.yaml
	$(KUBECTL) apply -f manifests/
	@echo "Restarting DaemonSet to pick up new image..."
	$(KUBECTL) rollout restart daemonset pcap-controller -n $(NAMESPACE)
	@echo "Waiting for rollout..."
	$(KUBECTL) rollout status daemonset pcap-controller -n $(NAMESPACE)

# ==============================================================================
# TESTING
# ==============================================================================
.PHONY: test-setup
test-setup: ## Create test namespace, pod, and install ping
	@echo "Setting up test environment..."
	$(KUBECTL) create ns $(TEST_NS) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) run test-pod --image=ubuntu:24.04 --namespace $(TEST_NS) -- sleep infinity
	@echo "Waiting for pod ready..."
	$(KUBECTL) wait --for=condition=Ready pod/test-pod -n $(TEST_NS) --timeout=60s
	@echo "Installing ping utility..."
	$(KUBECTL) exec -n $(TEST_NS) test-pod -- sh -c "apt-get update && apt-get install -y iputils-ping"
	@echo "Starting background traffic..."
	$(KUBECTL) exec -n $(TEST_NS) test-pod -- sh -c "ping 8.8.8.8 > /dev/null &"

.PHONY: verify
verify: ## Run the full verification test (Annotate -> Wait -> Check)
	@echo "--- STARTING VERIFICATION ---"
	@echo "1. Annotating pod for capture (5 packets)..."
	$(KUBECTL) annotate pod test-pod -n $(TEST_NS) "tcpdump.antrea.io=5" --overwrite
	@echo "2. Waiting 10s for capture..."
	@sleep 10
	@echo "3. Verifying output files..."
	@# This complex one-liner finds the specific controller pod on the same node
	@NODE=$$(kubectl get pod test-pod -n $(TEST_NS) -o jsonpath='{.spec.nodeName}'); \
	CPOD=$$(kubectl get pods -n $(NAMESPACE) -l app=pcap-controller --field-selector spec.nodeName=$$NODE -o jsonpath='{.items[0].metadata.name}'); \
	echo "Checking controller '$$CPOD' on node '$$NODE'..."; \
	kubectl exec -n $(NAMESPACE) $$CPOD -- ls -lh /captures
	@echo "--- VERIFICATION COMPLETE ---"

.PHONY: logs
logs: ## Tail logs of the controller
	@$(KUBECTL) logs -n $(NAMESPACE) -l app=pcap-controller -f

# ==============================================================================
# SHORTCUTS
# ==============================================================================
.PHONY: all
all: cluster-up deploy test-setup verify ## Full fresh start to verified test