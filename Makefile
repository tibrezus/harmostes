# harmostes — build, test, and image targets.
#
# Stack decision (see the wiki ADRs): the controller + worker runtime are Go
# (controller-runtime). Python/bash survive only as plugin scripts.

MODULE        := github.com/tibrezus/harmostes
IMG_CONTROLLER ?= ghcr.io/tibrezus/harmostes-controller
IMG_WORKER     ?= ghcr.io/tibrezus/harmostes-worker
IMG_UI         ?= ghcr.io/tibrezus/harmostes-ui
TAG           ?= dev

BIN_DIR       := bin
GO            := go

.PHONY: all build test test-ui vet tidy generate manifests controller-worker docker docker-push docker-ui clean

all: test build

## build: compile the CLI parity binary (the agent primitive, standalone).
build:
	$(GO) build -o $(BIN_DIR)/harmostes-agent ./cmd/harmostes-agent

## test: run all unit tests.
test:
	git submodule update --init --recursive
	$(GO) test ./...

## test-integration: integration tier — the attempt ledger + review-claim
## lifecycles against a REAL API server (envtest) with the chart CRDs
## applied. Catches CRD-schema pruning, which fake clients cannot see
## (#315). Needs envtest binaries once:
##   go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19 use 1.31.0 --bin-dir /tmp/envtest-bins
##   export KUBEBUILDER_ASSETS=/tmp/envtest-bins/k8s/1.31.0-linux-amd64
test-integration:
	$(GO) test -race -tags=integration ./test/integration/...

## test-ui: UI test framework — fixture-seeded goquery component tests
## plus the Playwright E2E tier against harmostes-ui -fixture.
## (e2e needs node/npm and Playwright browsers: npx playwright install chromium)
test-ui:
	$(GO) test ./internal/ui/... -run 'TestComponent|TestFixture'
	cd e2e && npm ci --silent && npx playwright test

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

## docker-ui: build the harmostes-ui image (includes SPA build stage).
docker-ui:
	docker build -t $(IMG_UI):$(TAG) -f Dockerfile.ui .

## web-build: build the React SPA and copy output to the embed path.
##   Run before `go build` or `go test ./internal/ui/` to test SPA routes locally.
web-build:
	cd web && npm ci && npm run build
	rm -rf internal/ui/static/spa/assets
	cp -r web/dist/* internal/ui/static/spa/

## web-dev: start the Vite dev server (hot reload, proxies /api to :8083).
web-dev:
	cd web && npm run dev

##: re-extract component CSS from the design system repo.
clean:
	rm -rf $(BIN_DIR)
