package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"log/slog"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/materialize"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// The materialized-runtime filesystem contract (#786, three-tier 1b). The
// init container writes; the run form reads. One set of literals, both sides
// — a drift here is a run whose pi loads a different extension set than the
// profile declared, so the constants live next to the functions that check
// them.
const (
	// ExtensionsRoot is the materialize init's install dir; the run form
	// expects one subdirectory per declared extension name.
	ExtensionsRoot = "/runtime/extensions"
	// HarnessRoot is the pinned runtime's install dir; .pi-path carries the
	// absolute pi executable path.
	HarnessRoot = "/runtime/harness"
	// PiPathFile is the file the harness materialization writes the
	// resolved pi binary path into.
	PiPathFile = HarnessRoot + "/.pi-path"
	// PluginsRoot is the aggregated plugin mount (the existing resolver
	// contract /plugins/<cm>/<name>.sh — unchanged).
	PluginsRoot = "/plugins"
	// PluginLabelKey is the label a ConfigMap carries to enter the
	// aggregated projection.
	PluginLabelKey = "harmostes.dev/plugin"
	// Env vars stamped by the dispatcher into both init and run containers.
	EnvExtensionManifest = "HARMOSTES_EXTENSION_MANIFEST"
	EnvHarnessManifest   = "HARMOSTES_HARNESS_MANIFEST"
	EnvAggregatePlugins  = "HARMOSTES_AGGREGATE_PLUGINS"
	EnvPluginLabelValue  = "HARMOSTES_PLUGIN_LABEL_VALUE"
	EnvPluginRefs        = "HARMOSTES_PLUGIN_REFS"
)

// RuntimeMaterialization is the run form's view of what the init container
// produced: the declared extensions' pi -e paths and (when the profile
// pinned a harness) the pi executable to spawn instead of the baked one.
type RuntimeMaterialization struct {
	ExtensionPaths []string // /runtime/extensions/<name>, order = manifest order
	PiPath         string   // empty = the image's baked pi
}

// RunInitRuntime is the init-runtime subcommand's body: consume the env the
// dispatcher stamped, materialize everything declared, write nothing on
// partial success (a failed entry refuses the whole init — half a runtime
// is worse than none, #504). Every failure returns a NAMED error.
func RunInitRuntime(ctx context.Context) error {
	cl, err := initRuntimeClient()
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	// Extensions: declared pins → fetch + verify + install, in order.
	pins, err := v1alpha1.ParseExtensionManifest(os.Getenv(EnvExtensionManifest))
	if err != nil {
		return err
	}
	for _, pin := range pins {
		if err := materialize.FetchExtension(ctx, registryFromEnv(), materialize.HTTPClient(), pin.Source, pin.Integrity, ExtensionsRoot); err != nil {
			return fmt.Errorf("extension %s: %w", pin.Name, err)
		}
	}
	if len(pins) > 0 {
		slog.Info("materialize: extensions verified + installed", "count", len(pins), "root", ExtensionsRoot)
	}

	// Harness: the pinned runtime (when declared).
	if h, err := v1alpha1.ParseHarnessManifest(os.Getenv(EnvHarnessManifest)); err != nil {
		return err
	} else if h != nil {
		if err := materialize.FetchHarness(ctx, registryFromEnv(), materialize.HTTPClient(), h.Source, h.Integrity, HarnessRoot); err != nil {
			return fmt.Errorf("harness %s: %w", h.Runtime, err)
		}
		slog.Info("materialize: harness verified + installed", "runtime", h.Runtime, "source", h.Source)
	}

	// Aggregated plugin mount: every labeled ConfigMap in the namespace.
	if os.Getenv(EnvAggregatePlugins) == "true" {
		labelValue := os.Getenv(EnvPluginLabelValue)
		if labelValue == "" {
			labelValue = "true"
		}
		n, err := materialize.ProjectPlugins(ctx, cl, os.Getenv("HARMOSTES_NAMESPACE"), labelValue, PluginsRoot)
		if err != nil {
			return err
		}
		slog.Info("materialize: plugin ConfigMaps projected", "count", n, "root", PluginsRoot)
	}
	// Explicit template payload refs (three-tier 1c): independent of the
	// label mode. An unknown ref refuses HERE — an explicit declaration
	// that does not resolve is a broken template, not an empty dir (#504).
	if refs := pluginRefsFromEnv(os.Getenv); len(refs) > 0 {
		n, err := materialize.ProjectPluginRefs(ctx, cl, os.Getenv("HARMOSTES_NAMESPACE"), refs, PluginsRoot)
		if err != nil {
			return err
		}
		slog.Info("materialize: plugin refs projected", "count", n, "root", PluginsRoot)
	}
	return nil
}

// pluginRefsFromEnv parses the comma-separated explicit plugin refs the Job
// stamps (HARMOSTES_PLUGIN_REFS). Empty segments ignored.
func pluginRefsFromEnv(getenv func(string) string) []string {
	raw := getenv(EnvPluginRefs)
	if raw == "" {
		return nil
	}
	var refs []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			refs = append(refs, name)
		}
	}
	return refs
}

// ConsumeMaterializedRuntime is the run form's half: validate that every
// DECLARED extension actually materialized (a declared-but-absent extension
// is a torn pod — the init failed half-way, or a volume mount moved; either
// way the run refuses loudly instead of loading a silently-smaller set,
// #504) and resolve the harness's pi path. Called AFTER the profile's model
// pin, BEFORE the graph compiles.
func ConsumeMaterializedRuntime(getenv func(string) string) (*RuntimeMaterialization, error) {
	return consumeMaterializedRuntime(getenv, ExtensionsRoot, HarnessRoot, PluginsRoot, os.Stat)
}

// consumeMaterializedRuntime is the injectable core (roots + stat for tests
// — the refusal paths must be testable without a /runtime on the host).
func consumeMaterializedRuntime(getenv func(string) string, extensionsRoot, harnessRoot, pluginsRoot string, stat func(string) (os.FileInfo, error)) (*RuntimeMaterialization, error) {
	out := &RuntimeMaterialization{}
	pins, err := v1alpha1.ParseExtensionManifest(getenv(EnvExtensionManifest))
	if err != nil {
		return nil, err
	}
	// Explicit plugin refs (three-tier 1c): a declared payload that did not
	// materialize is the same torn-pod class as a missing extension —
	// refuse, never run with a silently-smaller set (#504).
	for _, ref := range pluginRefsFromEnv(getenv) {
		dir := filepath.Join(pluginsRoot, ref)
		if _, err := stat(dir); err != nil {
			return nil, fmt.Errorf("declared plugin ref %s is not materialized at %s — the materialize init failed or did not run; refusing (a declared set must never silently shrink, #504)", ref, dir)
		}
	}
	for _, pin := range pins {
		dir := filepath.Join(extensionsRoot, pin.Name)
		if _, err := stat(dir); err != nil {
			return nil, fmt.Errorf("declared extension %s (%s) is not materialized at %s — the materialize init failed or did not run; refusing (a declared set must never silently shrink, #504)", pin.Name, pin.Source, dir)
		}
		out.ExtensionPaths = append(out.ExtensionPaths, dir)
	}
	if h, err := v1alpha1.ParseHarnessManifest(getenv(EnvHarnessManifest)); err != nil {
		return nil, err
	} else if h != nil {
		piPathFile := filepath.Join(harnessRoot, ".pi-path")
		raw, err := os.ReadFile(piPathFile)
		if err != nil {
			return nil, fmt.Errorf("harness %s is declared but %s is missing — the materialize init failed or did not run; refusing (#504)", h.Runtime, piPathFile)
		}
		piPath := strings.TrimSpace(string(raw))
		if piPath == "" {
			return nil, fmt.Errorf("harness %s: %s is empty — the materialize init wrote a pin without a binary; refusing (#504)", h.Runtime, piPathFile)
		}
		if _, err := os.Stat(piPath); err != nil {
			return nil, fmt.Errorf("harness %s: materialized pi %s is missing; refusing (#504)", h.Runtime, piPath)
		}
		out.PiPath = piPath
	}
	return out, nil
}

// registryFromEnv resolves the npm registry root (an air-gapped deployment
// points at its mirror; the default is the public registry).
func registryFromEnv() materialize.RegistryBase {
	if r := os.Getenv("HARMOSTES_NPM_REGISTRY"); r != "" {
		return materialize.RegistryBase(r)
	}
	return materialize.DefaultRegistry
}

// initRuntimeClient builds the in-cluster client for the plugin projection.
// The scheme is the worker's own (same registration the run form uses).
func initRuntimeClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: k8s.Scheme()})
}
