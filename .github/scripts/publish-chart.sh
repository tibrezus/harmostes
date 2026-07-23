#!/usr/bin/env bash
# Publish the harmostes OCI Helm chart, aligned to the release version.
#
# GoReleaser calls this as an after.hook with CHART_VERSION set to {{.Version}}.
# The chart version + image defaults are baked in so the published artifact is
# self-contained: chart version == appVersion == image tags.
#
# Auth: HARMOSTES_GITHUB_TOKEN (PAT — packages are user-owned, not org-owned).
set -euo pipefail

CHART_VERSION="${CHART_VERSION:?CHART_VERSION is required (set by GoReleaser)}"
TOKEN="${HARMOSTES_GITHUB_TOKEN:?HARMOSTES_GITHUB_TOKEN is required}"

echo "==> Aligning chart to version ${CHART_VERSION}"

# Bake the release version into the chart so the OCI artifact is self-contained.
yq -i ".version = \"${CHART_VERSION}\"" chart/Chart.yaml
yq -i ".appVersion = \"${CHART_VERSION}\"" chart/Chart.yaml
yq -i ".image.controller = \"ghcr.io/tibrezus/harmostes-controller:${CHART_VERSION}\"" chart/values.yaml
yq -i ".image.worker = \"ghcr.io/tibrezus/harmostes-worker:${CHART_VERSION}\"" chart/values.yaml
yq -i ".ui.image = \"ghcr.io/tibrezus/harmostes-ui:${CHART_VERSION}\"" chart/values.yaml

echo "--- Chart.yaml ---"
yq ".version, .appVersion" chart/Chart.yaml
echo "--- values.yaml (images) ---"
yq ".image, .ui.image" chart/values.yaml

echo "==> Packaging chart"
helm package chart/ --dependency-update

echo "==> Pushing OCI chart"
echo "${TOKEN}" | helm registry login ghcr.io -u x-access-token --password-stdin
helm push "harmostes-${CHART_VERSION}.tgz" oci://ghcr.io/tibrezus

echo "==> Chart published: oci://ghcr.io/tibrezus/harmostes:${CHART_VERSION}"
