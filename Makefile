CLUSTER_NAME := antrea-poormans-pcap
IMAGE_NAME   := pcap-controller:latest
NAMESPACE    := kube-system
TEST_NS      := test

KUBECTL := kubectl
KIND    := kind
DOCKER  := docker
HELM    := helm
GO      := go

.PHONY: help
help:
        @echo "Usage: make [target]"
        @echo ""
        @echo "Targets:"

.DEFAULT_GOAL := help

.PHONY: check-tools
check-tools:
        @command -v $(DOCKER) >/dev/null 2>&1 || { echo >&2 "Docker is required but not installed. Aborting."; exit 1; }
        @command -v $(KIND) >/dev/null 2>&1 || { echo >&2 "Kind is required but not installed. Aborting."; exit 1; }
        @command -v $(KUBECTL) >/dev/null 2>&1 || { echo >&2 "Kubectl is required but not installed. Aborting."; exit 1; }
        @command -v $(HELM) >/dev/null 2>&1 || { echo >&2 "Helm is required but not installed. Aborting."; exit 1; }
        @echo "All tools verified."

.PHONY: cluster-up
cluster-up: check-tools
        @echo "Creating Kind cluster '$(CLUSTER_NAME)'..."
        @$(KIND) get clusters | grep -q $(CLUSTER_NAME) || $(KIND) create cluster --name $(CLUSTER_NAME) --config kind-config.yaml
        @echo "Installing Antrea CNI..."
        @$(HELM) repo add antrea https://charts.antrea.io
        @$(HELM) repo update
        @$(HELM) list -n $(NAMESPACE) | grep -q antrea || $(HELM) install antrea antrea/antrea -n $(NAMESPACE) --wait

.PHONY: cluster-down
cluster-down:
        @echo "Deleting cluster '$(CLUSTER_NAME)'..."
        @$(KIND) delete cluster --name $(CLUSTER_NAME)
        @rm -rf /tmp/pcap-captures

.PHONY: fmt
fmt:
        $(GO) fmt ./...

.PHONY: vet
vet:
        $(GO) vet ./...

.PHONY: build
build:
        @echo "Building Docker image '$(IMAGE_NAME)'..."
        $(DOCKER) build -t $(IMAGE_NAME) .

.PHONY: load
load: build
        @echo "Loading image into cluster..."
        $(KIND) load docker-image $(IMAGE_NAME) --name $(CLUSTER_NAME)

.PHONY: deploy
deploy: load
        @echo "Deploying manifests..."
        $(KUBECTL) apply -f manifests/
        @echo "Restarting DaemonSet to pick up new image..."
        $(KUBECTL) rollout restart daemonset pcap-controller -n $(NAMESPACE)
        @echo "Waiting for rollout..."
        $(KUBECTL) rollout status daemonset pcap-controller -n $(NAMESPACE)

.PHONY: test-setup
test-setup:
        @echo "Setting up test environment..."
        $(KUBECTL) create ns $(TEST_NS) --dry-run=client -o yaml | $(KUBECTL) apply -f -
        $(KUBECTL) run test-pod --image=ubuntu:24.04 --namespace $(TEST_NS) -- sleep infinity
        @echo "Waiting for pod ready..."
        $(KUBECTL) wait --for=condition=Ready pod/test-pod -n $(TEST_NS) --timeout=60s
        @echo "Installing ping utility..."
        $(KUBECTL) exec -n $(TEST_NS) test-pod -- sh -c "apt-get update && apt-get install -y iputils-ping"
        @echo "Starting background traffic..."
        $(KUBECTL) exec -n $(TEST_NS) test-pod -- sh -c "nohup ping 8.8.8.8 > /dev/null 2>&1 &"

.PHONY: verify
verify:
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
logs:
        @$(KUBECTL) logs -n $(NAMESPACE) -l app=pcap-controller -f

.PHONY: all
all: cluster-up deploy test-setup verify
