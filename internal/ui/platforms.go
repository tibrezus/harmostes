package ui

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"sort"
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

// platformIDRe restricts platform identifiers to DNS-safe lowercase strings.
var platformIDRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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

// get returns the config for a platform ID. For unknown platforms, it
// auto-generates display metadata (Title-case label, hash-based color).
func (r *platformRegistry) get(id string) PlatformConfig {
	if c, ok := r.known[id]; ok {
		return c
	}
	// Auto-generate for unknown platforms.
	return PlatformConfig{
		ID:    id,
		Label: titleCase(id),
		Color: hashColor(id),
	}
}

// allKnown returns the operator-configured platforms in a stable order.
func (r *platformRegistry) allKnown() []PlatformConfig {
	result := make([]PlatformConfig, 0, len(r.known))
	for _, c := range r.known {
		result = append(result, c)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// mergeDiscovered returns all known platforms plus any platforms discovered
// from existing tokens that aren't in the known list. This is the full
// platform catalog for display.
func (r *platformRegistry) mergeDiscovered(discoveredIDs []string) []PlatformConfig {
	seen := make(map[string]bool, len(r.known))
	result := r.allKnown()
	for _, c := range result {
		seen[c.ID] = true
	}
	for _, id := range discoveredIDs {
		if !seen[id] && isValidPlatformID(id) {
			result = append(result, r.get(id))
			seen[id] = true
		}
	}
	return result
}

// isValidPlatformID checks that a platform identifier is a DNS-safe lowercase
// string. Any such string is accepted — platforms are NOT a fixed enum.
func isValidPlatformID(id string) bool {
	return platformIDRe.MatchString(id) && len(id) <= 63
}

// titleCase capitalizes the first letter, lowercasing the rest.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// hashColor generates a stable hex color from a string. Used for unknown
// platforms that don't have a configured color.
func hashColor(s string) string {
	h := md5.Sum([]byte(s))
	hashStr := hex.EncodeToString(h[:3])
	return "#" + hashStr
}
