package ui

import (
	"encoding/json"
	"os"
)

// PlatformConfig holds optional display metadata for a git platform. Platforms
// are NOT a fixed enum — any DNS-safe lowercase string is a valid platform ID
// (it's just a label on the token Secret). This config only provides display
// enrichment (label, color, icon URL) for platforms that want it. Unknown
// platforms are fully functional and get auto-generated display metadata.
type PlatformConfig struct {
	ID    string `json:"id"`    // DNS-safe identifier (e.g. "github", "bitbucket")
	Label string `json:"label"` // display name (defaults to Title(ID))
	Color string `json:"color"` // hex color for badges (auto-generated if empty)
	Icon  string `json:"icon"`  // optional URL to an icon/SVG
}

// DefaultPlatformConfigs provides display metadata for commonly-known git
// hosts. These are NOT required — they just make the UI prettier for platforms
// the operator expects. Unknown platforms are fully functional with
// auto-generated styling.
func DefaultPlatformConfigs() []PlatformConfig {
	return []PlatformConfig{
		{ID: "github", Label: "GitHub", Color: "#24292e"},
		{ID: "gitlab", Label: "GitLab", Color: "#fc6d26"},
		{ID: "forgejo", Label: "Forgejo", Color: "#fb8c00"},
		{ID: "codeberg", Label: "Codeberg", Color: "#2185d0"},
		{ID: "bitbucket", Label: "Bitbucket", Color: "#0052cc"},
		{ID: "gitea", Label: "Gitea", Color: "#609926"},
	}
}

// LoadPlatformConfigs reads platform display metadata from a JSON file (mounted
// as a ConfigMap in production). If the file is missing or unreadable, returns
// the defaults. This follows the same pattern as RBAC policy loading.
func LoadPlatformConfigs(path string) []PlatformConfig {
	if path == "" {
		return DefaultPlatformConfigs()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultPlatformConfigs()
	}
	var configs []PlatformConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return DefaultPlatformConfigs()
	}
	if len(configs) == 0 {
		return DefaultPlatformConfigs()
	}
	return configs
}

// platformRegistry holds the known platform configs and provides lookup +
// auto-generation for unknown platforms.
type platformRegistry struct {
	known map[string]PlatformConfig // id → config
}

func newPlatformRegistry(configs []PlatformConfig) *platformRegistry {
	r := &platformRegistry{known: make(map[string]PlatformConfig, len(configs))}
	for _, c := range configs {
		r.known[c.ID] = c
	}
	return r
}
