# harmostes — build, test, and image targets.
#
# Stack decision (see ARCHITECTURE.md): the controller + worker runtime are Go
# (controller-runtime). Python/bash survive only as plugin scripts.

MODULE        := github.com/tibrezus/harmostes
IMG_CONTROLLER ?= ghcr.io/tibrezus/harmostes-controller
IMG_WORKER     ?= ghcr.io/tibrezus/harmostes-worker
TAG           ?= dev

BIN_DIR       := bin
GO            := go

.PHONY: all build test vet tidy generate manifests controller-worker docker docker-push clean

all: test build

## build: compile the CLI parity binary (the agent primitive, standalone).
build:
	$(GO) build -o $(BIN_DIR)/harmostes-agent ./cmd/harmostes-agent

## test: run all unit tests.
test:
	git submodule update --init --recursive
	$(GO) test ./...

## vet: go vet.
vet:
	$(GO) vet ./...

## tidy: go mod tidy.
tidy:
	$(GO) mod tidy

## generate: regenerate DeepCopy + CRD with controller-gen (requires:
##   go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest).
##   NOTE: the harmostes CRD uses hand-maintained group registration (no
##   +kubebuilder:group markers), so controller-gen alone cannot reconstruct it
##   fully. The CRD at config/crd/workflows.harmostes.dev.yaml is the source of
##   truth; controller-gen output is a cross-check, not the generator.
generate: manifests
	controller-gen object paths="./api/..."

manifests:
	controller-gen crd paths="./api/..." output:dir=/tmp/crd-gen

## docker: build the multi-arch worker base image (Go worker binary + pi + plugin runtime).
##   Submodules (vendor/agents) must be initialised first — the Dockerfile COPYs
##   skills from there (single source of truth: tibrezus/agents).
docker:
	git submodule update --init --recursive
	docker build -t $(IMG_WORKER):$(TAG) -f Dockerfile.worker .

docker-push: docker
	docker push $(IMG_WORKER):$(TAG)

clean:
	rm -rf $(BIN_DIR)
