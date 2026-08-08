/*
 * flows.js — real-time flow table for harmostes observability UI.
 *
 * Subscribes to the global SSE stream and prepends new events to the table
 * as they arrive. Auto-scrolls unless the user is reading history.
 */

(function () {
	'use strict';

	window.initFlows = function (sseUrl) {
		var tbody = document.getElementById('flows-body');
		if (!tbody) return;

		var autoScroll = true;
		var maxRows = 200;

		// Detect user scroll to pause auto-scroll
		var container = document.querySelector('.flows-table-container');
		if (container) {
			container.addEventListener('scroll', function () {
				autoScroll = (container.scrollTop + container.clientHeight) >= (container.scrollHeight - 50);
			});
		}

		var es = new EventSource(sseUrl);
		es.onmessage = function (ev) {
			try {
				var data = JSON.parse(ev.data);
				if (!data || !data.event) return;

				// Skip pipeline-level events that don't have node info
				// (they're still useful, but we want to keep the table focused)
				var row = createRow(data);
				if (row) {
					// Prepend new rows (most recent at top)
					var firstChild = tbody.querySelector('.flows-row');
					if (firstChild) {
						tbody.insertBefore(row, firstChild);
					} else {
						// Remove empty state if present
						var empty = tbody.querySelector('.flows-empty');
						if (empty) empty.remove();
						tbody.appendChild(row);
					}

					// Trim to maxRows
					var rows = tbody.querySelectorAll('.flows-row');
					if (rows.length > maxRows) {
						for (var i = maxRows; i < rows.length; i++) {
							rows[i].remove();
						}
					}
				}
			} catch (e) {
				// ignore parse errors
			}
		};
	};

	function statusDotClass(status) {
		if (!status) return '';
		var s = status.toLowerCase();
		if (s === 'completed' || s === 'succeeded' || s === 'ok' || s === 'green') return 'ds-status-dot--positive';
		if (s === 'failed') return 'ds-status-dot--negative';
		if (s === 'running' || s === 'started') return 'ds-status-dot--warning';
		return '';
	}

	function formatTime(ts) {
		if (!ts) return '';
		var d = new Date(ts);
		if (isNaN(d.getTime())) return '';
		var h = String(d.getHours()).padStart(2, '0');
		var m = String(d.getMinutes()).padStart(2, '0');
		var s = String(d.getSeconds()).padStart(2, '0');
		return h + ':' + m + ':' + s;
	}

	function createRow(data) {
		var tr = document.createElement('tr');
		tr.className = 'flows-row';
		tr.setAttribute('data-workflow', data.pipeline || '');
		tr.setAttribute('data-status', data.status || '');

		var ts = data.timestamp ? formatTime(data.timestamp) : formatTime(new Date().toISOString());

		var dur = '';
		if (data.durationMs) {
			if (data.durationMs < 1000) dur = data.durationMs + 'ms';
			else dur = (data.durationMs / 1000).toFixed(1) + 's';
		}

		var status = data.status || '';
		var dotClass = statusDotClass(status);

		tr.innerHTML =
			'<td class="ds-table-td flows-ts">' + ts + '</td>' +
			'<td class="ds-table-td flows-wf">' + (data.pipeline || '') + '</td>' +
			'<td class="ds-table-td flows-node">' + (data.node || '—') + '</td>' +
			'<td class="ds-table-td flows-event">' + (data.event || '') + '</td>' +
			'<td class="ds-table-td flows-status">' +
				(dotClass ? '<span class="ds-status-dot ' + dotClass + '"></span>' : '<span class="ds-status-dot"></span>') +
				'<span class="flows-status-text">' + status + '</span>' +
			'</td>' +
			'<td class="ds-table-td flows-dur">' + dur + '</td>';

		return tr;
	}
})();
