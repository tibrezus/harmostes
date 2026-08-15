/*
 * map.js — Cytoscape.js topology graph for harmostes observability UI.
 *
 * Fetches the workflow's compiled graph from the API, renders it as an
 * interactive Cytoscape graph, and subscribes to SSE for live status
 * updates during active runs.
 */

(function () {
	'use strict';

	// Colours map to DS tokens (read from CSS custom properties at runtime).
	// CAUTION: tokens.css is written in oklch(), and Cytoscape's colour parser
	// does NOT understand modern CSS colour functions — an oklch() string fed
	// into a cytoscape style silently falls back to black (which once rendered
	// external nodes invisible on the dark canvas: "arrows pointing to
	// nothing"). So token() converts oklch()/oklab() to plain hex first.
	var tokenCache = {};

	function gamma(v) {
		v = v <= 0.0031308 ? 12.92 * v : 1.055 * Math.pow(v, 1 / 2.4) - 0.055;
		return Math.round(Math.min(1, Math.max(0, v)) * 255);
	}

	function oklabToHex(L, a, b) {
		var l_ = L + 0.3963377774 * a + 0.2158037573 * b;
		var m_ = L - 0.1055613458 * a - 0.0638541728 * b;
		var s_ = L - 0.0894841775 * a - 1.291485548 * b;
		var l = l_ * l_ * l_, m = m_ * m_ * m_, s = s_ * s_ * s_;
		var r = gamma(4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s);
		var g = gamma(-1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s);
		var bl = gamma(-0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s);
		return '#' + ((1 << 24) | (r << 16) | (g << 8) | bl).toString(16).slice(1);
	}

	// oklch(72% 0.005 85) → #999-ish hex; oklab(...) likewise; hex()/rgb()/named
	// pass through — those cytoscape parses natively. Percent L normalised to 0..1.
	function cssColorToHex(v) {
		v = v.trim();
		var m = v.match(/^oklch\(\s*([\d.]+)%?\s+([\d.eE+-]+)\s+([\d.eE+-]+)(?:\s*\/\s*[\d.]+%?)?\s*\)$/);
		if (m) {
			var L = parseFloat(m[1]) / (v.indexOf('%') === -1 ? 1 : 100);
			var C = parseFloat(m[2]), H = parseFloat(m[3]) * Math.PI / 180;
			return oklabToHex(L, C * Math.cos(H), C * Math.sin(H));
		}
		m = v.match(/^oklab\(\s*([\d.]+)%?\s+([\d.eE+-]+)\s+([\d.eE+-]+)(?:\s*\/\s*[\d.]+%?)?\s*\)$/);
		if (m) {
			var L2 = parseFloat(m[1]) / (v.indexOf('%') === -1 ? 1 : 100);
			return oklabToHex(L2, parseFloat(m[2]), parseFloat(m[3]));
		}
		return v;
	}

	function token(name) {
		if (tokenCache[name]) return tokenCache[name];
		var raw = getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#888';
		var hex = cssColorToHex(raw);
		tokenCache[name] = hex;
		return hex;
	}

	function isDark() {
		return document.documentElement.classList.contains('dark');
	}

	function nodeColor(status) {
		if (status === 'completed' || status === 'green') return token('--color-positive');
		if (status === 'failed') return token('--color-negative');
		if (status === 'running' || status === 'started') return token('--warning');
		if (status === 'external') return isDark() ? '#2a2d3a' : '#e9e7e2'; // visible ghost boxes
		return token('--border'); // pending/neutral
	}

	function nodeBorderColor(status) {
		if (status === 'completed' || status === 'green') return isDark() ? token('--color-positive-next') : token('--color-positive');
		if (status === 'failed') return isDark() ? token('--color-negative-next') : token('--color-negative');
		if (status === 'running' || status === 'started') return token('--color-warning-next') || token('--warning');
		return isDark() ? token('--color-next-light') : token('--color-rule');
	}

	function typeLabel(type) {
		var labels = {
			plugin: 'PLUGIN',
			agent: 'AGENT',
			gate: 'GATE',
			branch: 'BRANCH',
			'dapr-state-get': 'STATE GET',
			'dapr-state-set': 'STATE SET',
			'dapr-publish': 'PUBLISH',
			'vela-app': 'DEPLOY',
			'flux-reconcile': 'FLUX',
			'http-call': 'HTTP',
			'human-gate': 'HUMAN',
			external: 'EXTERNAL',
		};
		return labels[type] || type.toUpperCase();
	}

	// Convert harmostes GraphSpec (nodes/edges with from/to) to Cytoscape
	// elements (nodes/edges with source/target).
	function toCytoscape(graphData) {
		var elements = [];
		var nodeStatuses = (graphData.nodeStatuses || {});
		(graphData.graph.nodes || []).forEach(function (n) {
			var cfg = {};
			if (n.type === 'external' && n.config) {
				try { cfg = typeof n.config === 'string' ? JSON.parse(n.config) : n.config; } catch (e) { cfg = {}; }
			}
			elements.push({
				group: 'nodes',
				data: {
					id: n.id,
					label: n.label || n.id,
					nodeType: n.type,
					status: nodeStatuses[n.id] || (n.type === 'external' ? 'external' : 'pending'),
					description: cfg.description || '',
					trigger: cfg.trigger || '',
					component: cfg.component || '',
				},
			});
		});
		((graphData.graph.edges || []) || []).forEach(function (e) {
			elements.push({
				group: 'edges',
				data: {
					id: e.from + '->' + e.to,
					source: e.from,
					target: e.to,
					when: e.when || '',
				},
			});
		});
		return elements;
	}

	window.initHarmostesMap = function (containerId, graphUrl, sseUrl, templateMode) {
		var container = document.getElementById(containerId);
		if (!container) return;

		var cy = cytoscape({
			container: container,
			style: [
				{
					selector: 'node',
					style: {
						'label': 'data(label)',
						'text-valign': 'center',
						'text-halign': 'center',
						'color': isDark() ? token('--color-next-black') : '#fff',
						'text-wrap': 'wrap',
						'text-max-width': '120px',
						'font-family': 'Silkscreen, monospace',
						'font-size': '9px',
						'background-color': function (ele) {
							return nodeColor(ele.data('status'));
						},
						'border-width': 2,
						'border-color': function (ele) {
							return nodeBorderColor(ele.data('status'));
						},
						'width': 120,
						'height': 48,
						'shape': 'rectangle',
					},
				},
				{
					selector: 'edge',
					style: {
						'width': 2,
						'line-color': isDark() ? token('--color-next-light') : token('--color-rule'),
						'target-arrow-color': isDark() ? token('--color-next-light') : token('--color-rule'),
						'target-arrow-shape': 'triangle',
						'curve-style': 'bezier',
						'label': 'data(when)',
						'font-size': '7px',
						'font-family': 'VT323, monospace',
						'color': isDark() ? token('--color-next-light') : token('--color-ink-muted'),
						'text-background-color': isDark() ? token('--color-next-black') : token('--color-paper'),
						'text-background-padding': '2px',
					},
				},
				{
					selector: 'node[status = "running"]',
					style: {
						'border-style': 'dashed',
						'animation': 'pulse-border 2s linear infinite',
					},
				},
				// External (display-only) nodes: out-of-band systems — dashed
				// outline, transparent fill, muted italic label. They never
				// execute; the map renders them as conceptual topology.
				{
					selector: 'node[nodeType = "external"]',
					style: {
						// The fill must stay VISIBLE against the canvas in both themes
						// (they once rendered black-on-black via unparsed oklch tokens).
						'border-style': 'dashed',
						'border-width': 1.5,
						'background-color': isDark() ? '#2a2d3a' : '#e9e7e2',
						'color': isDark() ? token('--color-next-light') : token('--color-ink-muted'),
						'font-style': 'italic',
						'shape': 'round-rectangle',
						'border-color': isDark() ? token('--color-next-light') : token('--color-rule'),
					},
				},
			],
			layout: {
				name: 'breadthfirst',
				directed: true,
				padding: 24,
				spacingFactor: 1.3,
				nodeDimensionsIncludeLabels: true,
			},
			wheelSensitivity: 0.2,
		});

		// Node click → details panel
		var detailsPanel = document.getElementById('map-details');
		cy.on('tap', 'node', function (evt) {
			var node = evt.target;
			var data = node.data();
			if (detailsPanel) {
				detailsPanel.innerHTML =
					'<div class="map-details-header">' +
					'<span class="map-details-label">' + (data.label || data.id) + '</span>' +
					'<span class="ds-status-dot ds-status-dot--' + statusDotClass(data.status) + '"></span>' +
					'</div>' +
					'<div class="map-details-body">' +
					'<p><span class="detail-label">Type</span> <span class="detail-value">' + typeLabel(data.nodeType) + '</span></p>' +
					'<p><span class="detail-label">Status</span> <span class="detail-value">' + (data.nodeType === 'external' ? 'out-of-band' : (data.status || 'pending')) + '</span></p>' +
					'<p><span class="detail-label">Node ID</span> <span class="detail-value">' + data.id + '</span></p>' +
					(data.description ? '<p><span class="detail-label">Role</span> <span class="detail-value">' + data.description + '</span></p>' : '') +
					(data.trigger ? '<p><span class="detail-label">Triggered by</span> <span class="detail-value">' + data.trigger + '</span></p>' : '') +
					'</div>';
				detailsPanel.classList.add('map-details--open');
			}
		});

		// Click background → close panel
		cy.on('tap', function (evt) {
			if (evt.target === cy && detailsPanel) {
				detailsPanel.classList.remove('map-details--open');
			}
		});

		// Fetch graph data
		if (!graphUrl) {
			container.innerHTML =
				'<div class="ds-empty-state"><div class="ds-empty-text">' +
				'<p class="ds-empty-title">No workflow selected</p>' +
				'<p>Select a workflow from the top bar to see its topology graph.</p>' +
				'</div></div>';
			return;
		}

		fetch(graphUrl)
			.then(function (r) {
				if (!r.ok) throw new Error('graph API returned ' + r.status);
				return r.json();
			})
			.then(function (graphData) {
				var elements = toCytoscape(graphData);
				if (elements.length === 0) {
					container.innerHTML =
						'<div class="ds-empty-state"><div class="ds-empty-text">' +
						'<p class="ds-empty-title">Empty graph</p>' +
						'<p>This workflow has no compiled graph structure.</p>' +
						'</div></div>';
					return;
				}
				cy.add(elements);
				cy.layout({
					name: 'breadthfirst',
					directed: true,
					padding: 24,
					spacingFactor: 1.3,
					nodeDimensionsIncludeLabels: true,
				}).run();

				// If not template mode, subscribe to SSE for live status
				if (!templateMode && sseUrl) {
					subscribeSSE(sseUrl, cy);
				}
			})
			.catch(function (err) {
				container.innerHTML =
					'<div class="ds-empty-state"><div class="ds-empty-text">' +
					'<p class="ds-empty-title">Failed to load graph</p>' +
					'<p>' + err.message + '</p>' +
					'</div></div>';
			});
	};

	function statusDotClass(status) {
		if (status === 'completed' || status === 'green') return 'positive';
		if (status === 'failed') return 'negative';
		if (status === 'running' || status === 'started') return 'warning';
		return 'muted';
	}

	// Subscribe to SSE and update node statuses live
	function subscribeSSE(url, cy) {
		var es = new EventSource(url);
		es.onmessage = function (ev) {
			try {
				var data = JSON.parse(ev.data);
				if (!data || !data.event) return;

				// Map lifecycle events to node status
				var nodeId = data.node || data.nodeId;
				if (!nodeId) return;

				var status = null;
				if (data.event === 'node.started') status = 'running';
				else if (data.event === 'node.completed') status = 'completed';
				else if (data.event === 'node.failed') status = 'failed';

				if (status) {
					var node = cy.getElementById(nodeId);
					if (node && node.length > 0) {
						node.data('status', status);
					}
				}
			} catch (e) {
				// ignore parse errors
			}
		};
		es.onerror = function () {
			// SSE will auto-reconnect; nothing to do
		};
	}
})();
