# Changelog

## [1.11.1](https://github.com/tibrezus/harmostes/compare/v1.11.0...v1.11.1) (2026-11-01)


### Features

* **release:** patch-only automatic cuts — minors become milestone-driven (Release-As) ([#842](https://github.com/tibrezus/harmostes/issues/842)) ([efad9c7](https://github.com/tibrezus/harmostes/commit/efad9c7450aa00c74ef051197db77136671dbddd))

## [1.11.0](https://github.com/tibrezus/harmostes/compare/v1.10.0...v1.11.0) (2026-11-01)


### Features

* **fork-sync:** group defs — trust an aggregate PluginResult when the sync emits one ([#839](https://github.com/tibrezus/harmostes/issues/839)) ([41553d1](https://github.com/tibrezus/harmostes/commit/41553d14d8d8cf44a15d24d233d5a0ed0f87a576))

## [1.10.0](https://github.com/tibrezus/harmostes/compare/v1.9.5...v1.10.0) (2026-11-01)


### Features

* **tools:** the CR conformance tool moves into the kernel ([#832](https://github.com/tibrezus/harmostes/issues/832)) ([82e7372](https://github.com/tibrezus/harmostes/commit/82e7372b9cc25938e84bba90c5e53c01dc03580a))

## [1.9.5](https://github.com/tibrezus/harmostes/compare/v1.9.4...v1.9.5) (2026-10-31)


### Bug Fixes

* **skills:** bump the served-revision pin to live agents main ([#827](https://github.com/tibrezus/harmostes/issues/827)) ([bfe65fb](https://github.com/tibrezus/harmostes/commit/bfe65fbc64d0eeb6c98658530807553640d46756))

## [1.9.4](https://github.com/tibrezus/harmostes/compare/v1.9.3...v1.9.4) (2026-10-29)


### Bug Fixes

* **chart:** phase 4 decommission — the engine and every template leave the chart ([#823](https://github.com/tibrezus/harmostes/issues/823)) ([541b2ea](https://github.com/tibrezus/harmostes/commit/541b2ea3378d8ec04c8d625e73a647301e347c52))

## [1.9.3](https://github.com/tibrezus/harmostes/compare/v1.9.2...v1.9.3) (2026-10-29)


### Bug Fixes

* **chart:** forkMaintenance.payload chart|external — the engine payload writer moves out (three-tier 3a, [#789](https://github.com/tibrezus/harmostes/issues/789)) ([#817](https://github.com/tibrezus/harmostes/issues/817)) ([17215dc](https://github.com/tibrezus/harmostes/commit/17215dcc628c16bf218b28804339a83a5c924885))

## [1.9.2](https://github.com/tibrezus/harmostes/compare/v1.9.1...v1.9.2) (2026-10-28)


### Bug Fixes

* **fleet:** the thinking-wire extension crashed at load — the factory parameter rename missed the call site ([#814](https://github.com/tibrezus/harmostes/issues/814)) ([60f4801](https://github.com/tibrezus/harmostes/commit/60f4801932a434f3d721d0d4ec72006aca031952))

## [1.9.1](https://github.com/tibrezus/harmostes/compare/v1.9.0...v1.9.1) (2026-10-28)


### Bug Fixes

* **ui:** comment re-attachment applies once (1c live-verification follow-up) ([#810](https://github.com/tibrezus/harmostes/issues/810)) ([b176e44](https://github.com/tibrezus/harmostes/commit/b176e441434928dded0f4967fda91944ec1f7e0f))

## [1.9.0](https://github.com/tibrezus/harmostes/compare/v1.8.0...v1.9.0) (2026-10-28)


### Features

* **fleet:** the template's thinking knob becomes real on the wire — anthropic-object thinking for the mtplx-class engines ([#811](https://github.com/tibrezus/harmostes/issues/811)) ([60e4ba0](https://github.com/tibrezus/harmostes/commit/60e4ba09be6bbb9c2bd25b5ce92943a02ae5b6e0))

## [1.8.0](https://github.com/tibrezus/harmostes/compare/v1.7.1...v1.8.0) (2026-10-28)


### Features

* **three-tier 1c:** the Template CR becomes self-describing; the write path retargets ([#808](https://github.com/tibrezus/harmostes/issues/808)) ([727261a](https://github.com/tibrezus/harmostes/commit/727261aeed99465d6442758d658a8d5685e0940c))

## [1.7.1](https://github.com/tibrezus/harmostes/compare/v1.7.0...v1.7.1) (2026-10-28)


### Bug Fixes

* **worker:** the usage string knows init-runtime ([#786](https://github.com/tibrezus/harmostes/issues/786) follow-up) — the subcommand shipped but both help strings still said run|consumer ([#806](https://github.com/tibrezus/harmostes/issues/806)) ([0afa324](https://github.com/tibrezus/harmostes/commit/0afa324662490f24c2de0acbd0a0076da24fe89c))

## [1.7.0](https://github.com/tibrezus/harmostes/compare/v1.6.0...v1.7.0) (2026-10-28)


### Features

* **#2833:** the lineage carries the trace, not the transcript (ADR-0022) ([#805](https://github.com/tibrezus/harmostes/issues/805)) ([7a0c12b](https://github.com/tibrezus/harmostes/commit/7a0c12b4781baa3ff684512bcacc61b5d5ca591f))
* **release:** the release gate — patch-only automation, owner-gated minors ([#800](https://github.com/tibrezus/harmostes/issues/800)) ([10a8fbc](https://github.com/tibrezus/harmostes/commit/10a8fbc950cc90526f945cf7c4c2c5378c917db3))


### Bug Fixes

* **release:** gate provenance marker matches the bot's actual footer ([#803](https://github.com/tibrezus/harmostes/issues/803)) ([de2986b](https://github.com/tibrezus/harmostes/commit/de2986b54ea85b539030993cb2caa18d04d6956b))
* **release:** gate skips creation-time label events and unreadable bodies ([#802](https://github.com/tibrezus/harmostes/issues/802)) ([fa46206](https://github.com/tibrezus/harmostes/commit/fa462069e70aa7c4c05b61b8d3d372311a64c4fd))

## [1.6.0](https://github.com/tibrezus/harmostes/compare/v1.5.1...v1.6.0) (2026-10-26)


### Features

* **runtime:** the materialization seam — universal⊕template-delta extensions, harness pin, aggregated plugin mount ([#786](https://github.com/tibrezus/harmostes/issues/786)) ([#798](https://github.com/tibrezus/harmostes/issues/798)) ([fd7fc7f](https://github.com/tibrezus/harmostes/commit/fd7fc7f99a2509a4d6b679e0da1dffa04a4f4022))

## [1.5.1](https://github.com/tibrezus/harmostes/compare/v1.5.0...v1.5.1) (2026-10-26)


### Bug Fixes

* **chart:** the controller's dapr requirement and its sidecar are ONE contract ([#795](https://github.com/tibrezus/harmostes/issues/795)) ([6518e19](https://github.com/tibrezus/harmostes/commit/6518e19a30579b871fb887f340116919cf67ced1))
* **rbac:** grant events.events.k8s.io — the post-deprecation recorder writes v1 Events ([#797](https://github.com/tibrezus/harmostes/issues/797)) ([4b902ff](https://github.com/tibrezus/harmostes/commit/4b902ff917d35c463c520a41fdd4899f51276a09))

## [1.5.0](https://github.com/tibrezus/harmostes/compare/v1.4.1...v1.5.0) (2026-10-26)


### Features

* **#785:** AgentProfile CRD — the deployment's agentic decision surface (phase 1a, inert) ([#794](https://github.com/tibrezus/harmostes/issues/794)) ([9f8e0a3](https://github.com/tibrezus/harmostes/commit/9f8e0a3ff5e0e901a7d077b4bde29c53f4a414b9))

## [1.4.1](https://github.com/tibrezus/harmostes/compare/v1.4.0...v1.4.1) (2026-10-26)


### Features

* **#781:** the generic agent model — one knob for every workflow + the wired fallback for the fleet's pinned primary ([#781](https://github.com/tibrezus/harmostes/pull/781))

## [1.4.0](https://github.com/tibrezus/harmostes/compare/v1.3.6...v1.4.0) (2026-10-25)


### Features

* **#778:** the kernel ships logic, not decisions — config contract enforced at the render ([#779](https://github.com/tibrezus/harmostes/issues/779)) ([633bde0](https://github.com/tibrezus/harmostes/commit/633bde0e13fb8bb91b1c69ecda533ef78c66e533))

## [1.3.6](https://github.com/tibrezus/harmostes/compare/v1.3.5...v1.3.6) (2026-10-25)


### Bug Fixes

* **#704:** the release Dockerfile ships the plugins — both images pinned ([#774](https://github.com/tibrezus/harmostes/pull/774))

## [1.3.5](https://github.com/tibrezus/harmostes/compare/v1.3.4...v1.3.5) (2026-10-25)


### Bug Fixes

* **#704:** builtin-plugin image parity — coverage_carry.py and research_digest.py now ship ([#772](https://github.com/tibrezus/harmostes/pull/772))

## [1.3.4](https://github.com/tibrezus/harmostes/compare/v1.3.3...v1.3.4) (2026-10-25)


### Features

* **#725:** the compound is served, not searched for — research-prior.md at assembly + the one-context pillar frame ([#770](https://github.com/tibrezus/harmostes/pull/770))

## [1.3.3](https://github.com/tibrezus/harmostes/compare/v1.3.2...v1.3.3) (2026-10-25)


### Features

* **#749:** verdict_post refuses a second verdict at a head whose trailer already stands — the #567 currency mirrored tool-side ([#768](https://github.com/tibrezus/harmostes/pull/768))

## [1.3.2](https://github.com/tibrezus/harmostes/compare/v1.3.1...v1.3.2) (2026-10-25)


### Features

* **PR-REVIEW-7:** the review starts only when CI is green ([#766](https://github.com/tibrezus/harmostes/pull/766))

## [1.3.1](https://github.com/tibrezus/harmostes/compare/v1.3.0...v1.3.1) (2026-10-24)


### Features

* **#749 Phase 3:** post-review.sh deleted — the sweep consumes, the tools converge ([#763](https://github.com/tibrezus/harmostes/pull/763))

## [1.3.0](https://github.com/tibrezus/harmostes/compare/v1.2.1...v1.3.0) (2026-10-24)


### Features

* **release:** Conventional Commits + release-please, tag-driven GoReleaser ([#757](https://github.com/tibrezus/harmostes/issues/757)) ([#758](https://github.com/tibrezus/harmostes/issues/758)) ([6cdf2f5](https://github.com/tibrezus/harmostes/commit/6cdf2f5e0cd1ae13b45bf4aa01649a1b5c747537))
