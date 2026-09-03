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

	document.addEventListener('keydown', function (e) {
		if (e.key !== 'Enter' && e.key !== ' ') return;
		var exp = e.target.closest ? e.target.closest('.tbl .exp') : null;
		if (exp) {
			e.preventDefault();
			toggleGroup(exp);
		}
	});
})();
