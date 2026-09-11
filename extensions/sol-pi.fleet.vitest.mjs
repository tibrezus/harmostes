// Fleet-side vitest overlay for the vendored SoL-Pi suite (#425).
//
// Lives OUTSIDE extensions/sol-pi on purpose: the vendored tree stays
// byte-pristine against upstream (see extensions/sol-pi/UPSTREAM.md) so a
// bump is a directory replacement, not a patch series. This overlay carries
// the fleet's only suite delta:
//
//   - `package.test.ts` is excluded: it parses `npm pack` output, whose
//     notice format differs under npm 12 (env-only failure; verified
//     green on npm 10/11 upstream). The packaging surface it checks is
//     unused here — private package, loaded from the vendored tree.
//   - `install-guide.test.ts` is excluded: it validates the upstream
//     repo's agent-entry files (AGENTS.md / CLAUDE.md / root README),
//     which the vendored copy deliberately prunes (see UPSTREAM.md).
//   - exclude REPLACES vitest's defaults, so the stock node_modules/dist
//     entries are restated explicitly.
//
// Deliberately a plain object (no `import { defineConfig }`): this file
// sits outside extensions/sol-pi/node_modules, so `vitest` itself is not
// resolvable from here — and defineConfig is only typing sugar anyway.
//
// Run via `make test-sol-pi` (which pins the pi packages to PI_VERSION
// first — the PR-tier half of the compat pairing).
export default {
	test: {
		// Resolved against the vitest cwd (repo root — the make target cd-s
		// nowhere); relative form dodges import.meta.url shimming.
		root: ".",
		exclude: [
			"**/node_modules/**",
			"**/dist/**",
			"**/package.test.ts",
			"**/install-guide.test.ts",
		],
	},
};
