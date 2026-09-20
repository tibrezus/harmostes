// harmostes console interactions — delegated, framework-free.
// One listener per behavior, registered on the document: pages emit
// declarative markers (data-group, role="button"), this file owns the wiring.

(function () {
	function toggleGroup(el) {
		var gi = el.dataset.group;
		if (gi === undefined) return;
		var open = el.textContent === '▾';
		el.textContent = open ? '▸' : '▾';
		el.setAttribute('aria-expanded', String(!open));
		document.querySelectorAll('.tbl tr.subrow[data-group="' + gi + '"]').forEach(function (r) {
			r.classList.toggle('is-open', !open);
		});
	}

	document.addEventListener('click', function (e) {
		var exp = e.target.closest('.tbl .exp');
		if (exp) toggleGroup(exp);
	});

	// Run-log drawer (#551): the 3s live poll swaps only the inner line
	// list; compensate the scroller's scrollTop by the height delta so the
	// lines under the reader's eyes do NOT jump. Pinned-at-top readers
	// (newest first) stay pinned — new lines arrive where they look.
	document.body.addEventListener('htmx:beforeSwap', function (e) {
		var t = e.detail && e.detail.target;
		if (t && t.id === 'runlogs-lines') {
			var sc = document.getElementById('runlogs-scroll');
			if (sc) {
				sc.dataset.prevHeight = String(sc.scrollHeight);
				sc.dataset.prevTop = String(sc.scrollTop);
			}
		}
	});
	document.body.addEventListener('htmx:afterSwap', function (e) {
		var t = e.detail && e.detail.target;
		if (t && t.id === 'runlogs-lines') {
			var sc = document.getElementById('runlogs-scroll');
			if (sc && sc.dataset.prevHeight !== undefined) {
				var wasTop = Number(sc.dataset.prevTop) < 4;
				var delta = sc.scrollHeight - Number(sc.dataset.prevHeight);
				sc.scrollTop = wasTop ? 0 : (Number(sc.dataset.prevTop) + Math.max(delta, 0));
				delete sc.dataset.prevHeight;
				delete sc.dataset.prevTop;
			}
		}
	});

	// Drawer close (the fragment is replaced wholesale on the next open).
	document.addEventListener('click', function (e) {
		var btn = e.target.closest('.runlogs-close');
		if (!btn) return;
		var drawer = document.getElementById('runlogs-drawer');
		if (drawer) drawer.replaceChildren();
	});

	document.addEventListener('keydown', function (e) {
		if (e.key !== 'Enter' && e.key !== ' ') return;
		var exp = e.target.closest ? e.target.closest('.tbl .exp') : null;
		if (exp) {
			e.preventDefault();
			toggleGroup(exp);
		}
	});
})();
