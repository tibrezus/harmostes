/*
 * live.js — live agent TUI for harmostes observability UI.
 *
 * Subscribes to the pipeline SSE stream and renders events as terminal
 * output in real time. Styled as a retro terminal (always dark).
 */

(function () {
	'use strict';

	window.initLive = function (sseUrl, isActive) {
		var body = document.getElementById('live-terminal-body');
		var cursor = document.getElementById('live-cursor');
		if (!body) return;

		// Cursor blink
		if (cursor && isActive) {
			cursor.classList.add('ds-terminal-cursor--blink');
		} else if (cursor) {
			cursor.style.opacity = '0.3';
		}

		var es = new EventSource(sseUrl);
		es.onmessage = function (ev) {
			try {
				var data = JSON.parse(ev.data);
				if (!data || !data.event) return;
				appendEvent(body, data);
				// Auto-scroll to bottom
				body.scrollTop = body.scrollHeight;
			} catch (e) {
				// ignore parse errors
			}
		};
	};

	function appendEvent(body, data) {
		var line = document.createElement('div');
		line.className = 'ds-terminal-line';

		var ts = formatTime(data.timestamp);

		switch (data.event) {
			case 'pipeline.started':
				line.classList.add('ds-terminal-line--system');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> > pipeline started';
				break;

			case 'node.started':
				line.classList.add('ds-terminal-line--info');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> ◆ node <span class="ds-terminal-node">' + esc(data.node || '') + '</span> started';
				break;

			case 'node.completed':
				line.classList.add('ds-terminal-line--success');
				var dur = data.durationMs ? ' (' + formatDur(data.durationMs) + ')' : '';
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> ✓ node <span class="ds-terminal-node">' + esc(data.node || '') + '</span> completed' + dur;
				break;

			case 'node.failed':
				line.classList.add('ds-terminal-line--error');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> ✗ node <span class="ds-terminal-node">' + esc(data.node || '') + '</span> FAILED';
				if (data.feedback) {
					line.innerHTML += '<div class="ds-terminal-sub">' + esc(data.feedback) + '</div>';
				}
				break;

			case 'pipeline.completed':
				line.classList.add('ds-terminal-line--success');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> <span class="ds-terminal-green">◆ pipeline completed: GREEN</span>';
				break;

			case 'pipeline.failed':
				line.classList.add('ds-terminal-line--error');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> <span class="ds-terminal-red">◆ pipeline completed: FAILED</span>';
				break;

			default:
				// Generic event
				line.classList.add('ds-terminal-line--info');
				line.innerHTML = '<span class="ds-terminal-ts">' + ts + '</span> ' + esc(data.event) + (data.node ? ' · ' + esc(data.node) : '');
		}

		body.appendChild(line);
	}

	function formatTime(ts) {
		if (!ts) return '';
		var d = new Date(ts);
		if (isNaN(d.getTime())) return '';
		return String(d.getHours()).padStart(2, '0') + ':' +
			String(d.getMinutes()).padStart(2, '0') + ':' +
			String(d.getSeconds()).padStart(2, '0');
	}

	function formatDur(ms) {
		if (ms < 1000) return ms + 'ms';
		return (ms / 1000).toFixed(1) + 's';
	}

	function esc(s) {
		var div = document.createElement('div');
		div.textContent = s;
		return div.innerHTML;
	}
})();
