/*
 * metrics.js — token usage histogram for harmostes observability UI.
 *
 * Fetches token metrics from the SigNoz API and renders an inline SVG
 * stacked histogram (input/output/cacheRead vs time).
 */

(function () {
	'use strict';

	function token(name) {
		return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#888';
	}

	function isDark() {
		return document.documentElement.classList.contains('dark');
	}

	function resolveColor(varName) {
		var v = token(varName);
		if (v.startsWith('oklch')) return v; // modern browsers support oklch in SVG
		return v;
	}

	window.initMetrics = function (apiUrl) {
		var container = document.getElementById('metrics-histogram');
		if (!container) return;

		container.innerHTML = '<p class="metrics-loading">Loading metrics…</p>';

		fetch(apiUrl)
			.then(function (r) {
				if (!r.ok) throw new Error('API returned ' + r.status);
				return r.json();
			})
			.then(function (data) {
				renderHistogram(container, data);
			})
			.catch(function (err) {
				container.innerHTML = '<div class="ds-empty-state"><div class="ds-empty-text">' +
					'<p class="ds-empty-title">Metrics unavailable</p>' +
					'<p>' + err.message + '</p>' +
					'</div></div>';
			});
	};

	function renderHistogram(container, data) {
		var series = data.series || [];
		if (series.length === 0 || series.every(function (s) { return !s.points || s.points.length === 0; })) {
			container.innerHTML = '<div class="ds-empty-state"><div class="ds-empty-text">' +
				'<p class="ds-empty-title">No token data</p>' +
				'<p>No agent telemetry recorded for this time range. Deterministic workflows (e.g. <code>fork-maintenance</code>) don\'t invoke an LLM and have no tokens to report.</p>' +
				'</div></div>';
			return;
		}

		// Use the first series with points to determine buckets
		var refSeries = series.find(function (s) { return s.points && s.points.length > 0; }) || series[0];
		var numBuckets = refSeries.points.length;

		var width = 760;
		var height = 280;
		var padL = 50;
		var padR = 20;
		var padT = 20;
		var padB = 40;
		var chartW = width - padL - padR;
		var chartH = height - padT - padB;

		var barW = Math.floor(chartW / numBuckets);
		if (barW < 4) barW = 4;
		if (barW > 36) barW = 36;

		// Find max stacked value
		var maxVal = 0;
		for (var i = 0; i < numBuckets; i++) {
			var sum = 0;
			series.forEach(function (s) {
				if (s.points && i < s.points.length) sum += s.points[i].value;
			});
			if (sum > maxVal) maxVal = sum;
		}
		if (maxVal === 0) maxVal = 1;

		var gridColor = isDark() ? token('--color-next-mid') : token('--color-rule');
		var fgMuted = isDark() ? token('--color-next-light') : token('--color-ink-muted');

		var svg = '<svg viewBox="0 0 ' + width + ' ' + height + '" class="metrics-svg" xmlns="http://www.w3.org/2000/svg">';
		svg += '<style>.metrics-svg text{font-family:VT323,monospace;font-size:11px;fill:' + fgMuted + '}</style>';

		// Y-axis grid + labels
		for (var g = 0; g <= 4; g++) {
			var gy = padT + (chartH * g) / 4;
			var gval = maxVal * (4 - g) / 4;
			svg += '<line x1="' + padL + '" y1="' + gy + '" x2="' + (width - padR) + '" y2="' + gy + '" stroke="' + gridColor + '" stroke-width="0.5"/>';
			svg += '<text x="' + (padL - 4) + '" y="' + (gy + 3) + '" text-anchor="end">' + formatNumber(gval) + '</text>';
		}

		// Stacked bars
		for (var b = 0; b < numBuckets; b++) {
			var bx = padL + b * barW;
			var stackY = padT + chartH;

			series.forEach(function (s) {
				if (!s.points || b >= s.points.length) return;
				var val = s.points[b].value;
				if (val <= 0) return;
				var barH = (val / maxVal) * chartH;
				var by = stackY - barH;
				var color = resolveColor(s.color);
				svg += '<rect x="' + bx + '" y="' + by.toFixed(1) + '" width="' + (barW - 1) + '" height="' + barH.toFixed(1) + '" fill="' + color + '"/>';
				stackY = by;
			});
		}

		// X-axis timestamps
		if (refSeries.points.length > 0) {
			var first = formatTime(refSeries.points[0].timestamp);
			var last = formatTime(refSeries.points[refSeries.points.length - 1].timestamp);
			svg += '<text x="' + padL + '" y="' + (height - padB + 16) + '">' + first + '</text>';
			svg += '<text x="' + (width - padR) + '" y="' + (height - padB + 16) + '" text-anchor="end">' + last + '</text>';
		}

		// Axis labels
		svg += '<text x="' + (padL - 40) + '" y="' + (padT + chartH / 2) + '" transform="rotate(-90 ' + (padL - 40) + ' ' + (padT + chartH / 2) + ')" text-anchor="middle">tokens</text>';
		svg += '<text x="' + (padL + chartW / 2) + '" y="' + (height - 4) + '" text-anchor="middle">time</text>';

		svg += '</svg>';

		// Legend
		var legend = '<div class="metrics-legend">';
		series.forEach(function (s) {
			legend += '<span class="metrics-legend-item"><span class="metrics-legend-dot" style="background:' + resolveColor(s.color) + '"></span>' + s.label + '</span>';
		});
		legend += '</div>';

		container.innerHTML = legend + svg;
	}

	function formatTime(ts) {
		var d = new Date(ts);
		if (isNaN(d.getTime())) return '';
		return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
	}

	function formatNumber(n) {
		if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
		if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
		return Math.round(n).toString();
	}
})();
