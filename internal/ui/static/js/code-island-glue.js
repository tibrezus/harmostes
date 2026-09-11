// Workflow Code island glue (issue #416, ADR-0012 §2) — the application's
// hand-written half of the island. The vendored bundle
// (static/vendor/code-island/code-island.js) is pure third-party library;
// everything the console itself decides lives here: fetching the CRD-derived
// schema from /api/schema, mapping tokens.css onto the editor theme, and
// exposing window.harmostesCodeIsland for the future MR-bridge (#418) and
// the Playwright tier.
//
// Read-only mode first (#416): the island renders the template document with
// schema completion + validation markers; editing lands with the inspector.
(function () {
	'use strict'

	function cssVar(name) {
		return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
	}

	// Monaco theme colors accept only #rrggbb — tokens.css speaks oklch.
	// Convert (with gamut clamping) so the token file stays the single
	// color source; a future retune needs no island change.
	function toHex(v) {
		var m = /^oklch\(([\d.]+)%?\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*[\d.]+)?\)$/.exec(v.replace(/\s+/g, ' '))
		if (!m) return v
		var L = parseFloat(m[1]) / 100, C = parseFloat(m[2]), H = parseFloat(m[3]) * Math.PI / 180
		var a = Math.cos(H) * C, b = Math.sin(H) * C
		var l_ = L + 0.3963377774 * a + 0.2158037573 * b
		var m_ = L - 0.1055613458 * a - 0.0638541728 * b
		var s_ = L - 0.0894841775 * a - 1.2914855480 * b
		var l = l_ * l_ * l_, mm = m_ * m_ * m_, ss = s_ * s_ * s_
		var rgb = [
			4.0767416621 * l - 3.3077115913 * mm + 0.2309699292 * ss,
			-1.2684380046 * l + 2.6097574011 * mm - 0.3413193965 * ss,
			-0.0041960863 * l - 0.7034186147 * mm + 1.7076147010 * ss,
		].map(function (c) {
			c = Math.round(Math.min(1, Math.max(0, c)) * 255)
			return (c < 16 ? '0' : '') + c.toString(16)
		})
		return '#' + rgb.join('')
	}

	// Palette derived live from tokens.css — the island inherits theme
	// changes (light/dark toggle, future token retunes) without its own
	// color literals. Contract: read the tokens, never rename them.
	function palette() {
		return {
			dark: document.documentElement.classList.contains('dark'),
			bg: toHex(cssVar('--bg-alt')),
			elevated: toHex(cssVar('--bg')),
			elevatedHover: toHex(cssVar('--bg-tertiary')),
			fg: toHex(cssVar('--fg')),
			muted: toHex(cssVar('--fg-muted')),
			border: toHex(cssVar('--border')),
			accent: toHex(cssVar('--accent')),
			string: toHex(cssVar('--success')),
			warning: toHex(cssVar('--warning')),
			danger: toHex(cssVar('--danger')),
			scrollbar: toHex(cssVar('--border')),
			mono: cssVar('--font-mono'),
			fontSize: 12.5,
		}
	}

	var handle = null

	// mountIsland arms one island on the page. Schema unreachable ⇒ the
	// document still renders, validation stays silent, the state attribute
	// says so — degraded, never blank.
	function mountIsland(el, text, modelPath, schemaKey, schemaUri) {
		var readOnly = el.getAttribute('data-readonly') === 'true'
		var common = {
			el: el,
			text: text,
			modelPath: modelPath,
			readOnly: readOnly,
			assetVersion: window.__assetVersion || '',
			palette: palette(),
		}
		if (!window.HarmostesCodeIsland) {
			el.setAttribute('data-island-state', 'library-missing')
			return
		}
		fetch('/api/schema', { headers: { Accept: 'application/json' } })
			.then(function (r) {
				if (!r.ok) throw new Error('schema ' + r.status)
				return r.json()
			})
			.then(
				function (schemas) {
					handle = window.HarmostesCodeIsland.mount(Object.assign({}, common, {
						schemaUri: schemaUri,
						schema: schemas[schemaKey] || null,
					}))
					el.setAttribute('data-island-state', 'ready')
				},
				function (err) {
					el.setAttribute('data-island-error', String(err))
					handle = window.HarmostesCodeIsland.mount(common)
					el.setAttribute('data-island-state', 'schema-error')
				},
			)
		// The handle materialises asynchronously; the e2e tier (and the
		// MR-bridge) need it directly once mount completed — a lazy getter
		// forwards to whatever the current handle is.
		Object.defineProperty(window, 'harmostesCodeIsland', {
			configurable: true,
			get: function () { return handle },
		})
	}

	// The theme toggle flips html.dark; re-map the palette in place.
	document.addEventListener('harmostes:theme-changed', function () {
		if (handle) handle.setTheme(palette())
	})

	window.harmostesMountCodeIsland = mountIsland
})()
