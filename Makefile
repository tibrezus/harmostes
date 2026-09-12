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

.PHONY: all build test test-go test-ui vet tidy generate manifests controller-worker docker docker-push docker-ui test-extensions test-sol-pi test-integration clean test-rig-emit golden-update


all: test build

## build: compile the CLI parity binary (the agent primitive, standalone).
build:
	$(GO) build -o $(BIN_DIR)/harmostes-agent ./cmd/harmostes-agent

## test: every tier. Tiers degrade independently (#338 r24 D3): test-go needs
## only the Go toolchain; test-extensions adds Node ≥ 22.5 + npm + python3
## (the fixture producer). CI runs them as separate steps, so a host without
## Node still gets a meaningful `make test-go`.
# test-sol-pi is deliberately NOT here: it is the slow compat tier (installs
# a second pi package set), run explicitly by CI next to the extensions tier.
test: test-go test-extensions test-rig-emit


## test-go: the Go tier alone.
test-go:
	git submodule update --init --recursive
	$(GO) test ./...

## golden-update: regenerate the committed golden render (ADR-0011 #368-8).
## Renders chart/ with the fictional production-shaped fixture
## (chart/ci/golden-values.yaml) into chart/ci/golden/full.yaml. CI re-renders
## and diffs — any rendered-output change, intended or accidental, shows up
## as a reviewable diff instead of a runtime surprise. Chart-version labels
## are filtered (they change every release; `make golden-update` after any
## template or payload change).
##
## HELM VERSION SENSITIVITY: the render is byte-sensitive to helm's
## whitespace behavior (a v4.2.2→v4.2.4 patch changed trailing-newline
## emission). CI pins the helm version in .github/workflows/ci.yml —
## regenerate the golden under THAT version when bumping it. If make
## golden-update and CI disagree with zero local diff, check helm versions
## first (helm version --short).
golden-update:
	helm template harmostes chart/ -f chart/ci/golden-values.yaml \
		| grep -vE '(helm\.sh/chart|app\.kubernetes\.io/version):' > chart/ci/golden/full.yaml

## test-extensions: the pi extensions' TypeScript (rig-query) — the query
## layer is pure TS over rig.db; its fixture suite runs under node --test
## with type stripping (requires Node ≥ 22.5 — the same runtime the worker
## image ships). The fixture is regenerated with the REAL producer
## (python3 extensions/rig-query/fixtures/generate.py) and committed; CI
## regenerates and fails on drift.
test-extensions:
	npm ci --prefix extensions/rig-query --no-audit --no-fund --silent
	node --test --experimental-strip-types \
		extensions/rig-query/queries.test.ts \
		extensions/rig-query/index.parse.test.ts \
		extensions/rig-query/index.runtime.test.ts \
		extensions/litellm-provider/fallbacks.test.ts
	@node --experimental-strip-types -e 'await import("./extensions/litellm-provider/index.ts")'
	python3 extensions/rig-query/fixtures/freshness.py
	@# Chart copy drift gate: the resolver's litellm-provider ConfigMap source
	@# (chart/files/litellm-provider/) is a pinned copy of the canonical
	@# extension — Helm .Files cannot reach outside chart/. Fail on drift so
	@# the copy can never silently go stale (ADR-0011).
	@for f in extensions/litellm-provider/*; do \
	  n=$$(basename $$f); \
	  cmp -s $$f chart/files/litellm-provider/$$n || { echo "DRIFT/MISSING: chart/files/litellm-provider/$$n vs extensions/litellm-provider/ — re-copy the canonical source" >&2; exit 1; }; \
	done; \
	for f in chart/files/litellm-provider/*; do \
	  n=$$(basename $$f); \
	  [ -e extensions/litellm-provider/$$n ] || { echo "EXTRA: chart/files/litellm-provider/$$n has no canonical counterpart — stale copy" >&2; exit 1; }; \
	done; \
	echo "litellm-provider chart copy: complete + in sync with extensions/litellm-provider/"

# sol-pi (#425): the vendored NVlabs/SoL-Pi suite run against the FLEET's pi
# packages, not the vendored lockfile's 0.84.2 dev-deps — the PR-tier half of
# the compat pairing (image-tier half: the Dockerfile load probe). PI_VERSION
# is DERIVED from Dockerfile.worker (the hand-pin's single source — no fourth
# literal; a bump re-runs this tier against the new pi automatically).
PI_VERSION ?= $(shell sed -n 's/^ARG PI_VERSION=//p' Dockerfile.worker | head -1)
## test-sol-pi: the SoL-Pi compat tier. Deliberately NOT part of `test:` — it
## installs a second pi package set (slow) and exists to catch a compatibility
## change (upstream agents-install protocol) that the fast tier cannot see.
## CI runs it explicitly next to the extensions tier (ci.yml); run by hand on
## any PI_VERSION or vendored-tree bump.
test-sol-pi:
	npm ci --prefix extensions/sol-pi --ignore-scripts --no-audit --no-fund --silent
	npm install --prefix extensions/sol-pi --no-save --no-audit --no-fund --silent \
		@earendil-works/pi-coding-agent@$(PI_VERSION) \
		@earendil-works/pi-ai@$(PI_VERSION) \
		@earendil-works/pi-agent-core@$(PI_VERSION) \
		@earendil-works/pi-tui@$(PI_VERSION)
	# The vitest overlay (repo-owned, outside the vendored tree) excludes
	# package.test.ts — it parses `npm pack` output, whose notice format
	# differs under npm 12 (env-only failure; the packaging surface it
	# checks is unused here — private package, loaded from the tree) — and
	# install-guide.test.ts (validates the upstream repo's agent-entry
	# files, pruned in the vendored copy). cwd = the package root: several
	# upstream tests resolve scripts/docs against process.cwd() (upstream
	# layout); the overlay path is relative to that cwd.
	cd extensions/sol-pi && npx vitest run --config ../sol-pi.fleet.vitest.mjs
	@echo "sol-pi compat tier green (pi $(PI_VERSION))"

## test-rig-emit: the rig-emit plugin's Python validator — severity pin:
## circular deps WARN (the graph represents the codebase as it is; failing
## the emit left reviews graph-less, rhesadox#1864), the rest stay errors.
## Plus rig-brief's contract tests (#443): the prepare-time context
## enrichment must map changed files → components, name symbols with
## file:line, compute blast radius, and fail open.
test-rig-emit:
	python3 plugins/rig-emit/test_validator.py
	python3 plugins/rig-emit/test_brief.py

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
