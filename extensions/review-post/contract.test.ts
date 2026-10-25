/**
 * review-post contract tests (#749 Phase 1).
 *
 * Runs under plain node (--experimental-strip-types, typebox from
 * devDependencies — the rig-query/pillar-todo pattern: NO pi closure in
 * the test tier). The host is a fake; what is under test is the
 * CONTRACT, not any forge:
 *
 *   1. thread_ref is returned ONLY from a host 2xx — a failed post
 *      yields an error result and NO ref, and never mutates state.
 *   2. No code path emits a verdict EVENT: finding posts are event
 *      COMMENT with exactly one anchored comment; the verdict is an
 *      issue comment carrying the trailer. (#680 by code.)
 *   3. Resume: branch state (posted findings, verdict) reconstructs
 *      from toolResult details; a re-round never double-posts.
 *   4. Fail-closed arming: absent credential or absent pr-context →
 *      every tool degrades to the ledger message, nothing throws.
 *   5. The verdict trailer is exact: `<!-- pr-review: DECISION @ sha -->`.
 */

import assert from "node:assert/strict";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
import { test, mock, afterEach } from "node:test";
import { resolveTarget, default as reviewPost } from "./index.ts";

// ── fakes ────────────────────────────────────────────────────────────────

interface Call {
	method: string;
	path: string;
	body?: any;
}

/** Fake host: programmable responses, records every call. */
function fakeHost(responses: Map<string, { status: number; json: any }>) {
	const calls: Call[] = [];
	const fetchMock = async (_url: any, init: any) => {
		const path = String(_url).replace(/^https:\/\/[^/]+(\/api\/v1)?/, "");
		const call: Call = { method: init?.method ?? "GET", path, body: init?.body ? JSON.parse(init.body) : undefined };
		calls.push(call);
		const key = `${call.method} ${path.split("?")[0]}`;
		const r = responses.get(key) ?? { status: 404, json: { message: "not found" } };
		return {
			status: r.status,
			json: async () => r.json,
		};
	};
	return { calls, fetchMock };
}

const CLEANUPS: (() => void)[] = [];
afterEach(() => { for (const c of CLEANUPS.splice(0)) c(); });

const CTX = { host: "git.rezus.cloud", repo: "tibrez/rhesadox", number: 2833, head_sha: "bf994bb6" };
const ENV = { HARMOSTES_FORGEJO_TOKEN: "tok", HARMOSTES_WORKDIR: "/workspace" };

/** Harness: install the extension against a fake host + fake pi session. */
function harness(opts: { env?: any; ctx?: any; responses?: Map<string, { status: number; json: any }> } = {}) {
	const env = { ...ENV, ...opts.env };
	const prContext = opts.ctx === null ? null : { ...CTX, ...opts.ctx };
	const fake = fakeHost(opts.responses ?? new Map());
	const undici = mock.method(globalThis, "fetch", fake.fetchMock as any);
	// pr-context reader: point HARMOSTES_WORKDIR at a temp dir with the file
	const fs = require("node:fs");
	const os = require("node:os");
	const path = require("node:path");
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "rp-"));
	fs.writeFileSync(path.join(dir, "pr-context.json"), JSON.stringify(prContext));
	// the extension reads process.env directly (env is a runtime fact) —
	// stub the names it consumes, restore on undici.mock.restore()
	const saved = {
		WORKDIR: process.env.HARMOSTES_WORKDIR,
		FJ: process.env.HARMOSTES_FORGEJO_TOKEN,
		GH: process.env.HARMOSTES_REVIEW_POST_TOKEN,
	};
	process.env.HARMOSTES_WORKDIR = dir;
	process.env.HARMOSTES_FORGEJO_TOKEN = env.HARMOSTES_FORGEJO_TOKEN ?? "";
	process.env.HARMOSTES_REVIEW_POST_TOKEN = env.HARMOSTES_REVIEW_POST_TOKEN ?? "";
	const tools: Map<string, (id: string, params: any) => Promise<any>> = new Map();
	const handlers: Map<string, (event: string, ctx: any) => Promise<void>> = new Map();
	const fakePi = {
		on: (event: string, handler: (e: any, ctx: any) => Promise<void>) => handlers.set(event, handler),
		registerTool: (spec: any) => tools.set(spec.name, spec.execute),
	};
	reviewPost(fakePi as any);
	const sessionManager = { getBranch: () => opts.branch ?? [] };
	const api = {
		env,
		workdir: dir,
		handlers,
		tools,
		call: (name: string, params: any = {}) => tools.get(name)!("id", params),
		start: () => handlers.get("session_start")!({}, { sessionManager }),
		done: () => {
			undici.mock.restore();
			process.env.HARMOSTES_WORKDIR = saved.WORKDIR;
			process.env.HARMOSTES_FORGEJO_TOKEN = saved.FJ;
			process.env.HARMOSTES_REVIEW_POST_TOKEN = saved.GH;
		},
	};
	CLEANUPS.push(() => api.done());
	return { api, fake, undici };
}

const OK_REVIEW = { status: 200, json: { id: 1407 } };
const OK_COMMENT = { status: 200, json: [{ id: 99, path: "a.zig", body: "x" }] };

function baseResponses(): Map<string, { status: number; json: any }> {
	return new Map([
		["POST /repos/tibrez/rhesadox/pulls/2833/reviews", OK_REVIEW],
		["GET /repos/tibrez/rhesadox/pulls/2833/reviews/1407/comments", OK_COMMENT],
		["POST /repos/tibrez/rhesadox/issues/2833/comments", { status: 200, json: { id: 5001 } }],
		// the moved-head guard reads the live head on every verdict
		["GET /repos/tibrez/rhesadox/pulls/2833", { status: 200, json: { head: { sha: "bf994bb6" } } }],
		// the verdict-currency lookup reads the conversation (no trailer = no standing verdict)
		["GET /repos/tibrez/rhesadox/issues/2833/comments", { status: 200, json: [] }],
	]);
}

const FINDING = { path: "src/a.zig", line: 42, severity: "Blocking", body: "the guard greps a line this row cannot emit" };

// ── 1. thread_ref only from host 2xx ─────────────────────────────────────

test("finding_post: happy path returns the host-issued thread_ref and records it", async () => {
	const h = harness({ responses: baseResponses() });
	try {
		await h.api.start();
		const r = await h.api.call("review_finding_post", FINDING);
		const d = r.details;
		assert.equal(d.posted.length, 1);
		assert.equal(d.posted[0].thread_ref, "forgejo:tibrez/rhesadox#2833:r1407:c99");
		// the wire shape: ONE review event, COMMENT, one anchored comment
		const post = h.fake.calls.find((c) => c.method === "POST" && c.path.endsWith("/reviews"));
		assert.equal(post.body.event, "COMMENT");
		assert.equal(post.body.commit_id, "bf994bb6");
		assert.equal(post.body.comments.length, 1);
		assert.equal(post.body.comments[0].new_position, 42);
	} finally {
		h.api.done();
	}
});

test("finding_post: host failure — error result, NO thread_ref, no state mutation", async () => {
	const responses = new Map([["POST /repos/tibrez/rhesadox/pulls/2833/reviews", { status: 403, json: { message: "forbidden" } }]]);
	const h = harness({ responses });
	await h.api.start();
	const r = await h.api.call("review_finding_post", FINDING);
	assert.match(String(r.content[0].text), /post failed/);
	assert.equal(r.details.posted.length, 0);
	assert.ok(r.details.error);
	// and it did NOT fabricate a call to look up comments (no 2xx happened)
	assert.equal(h.fake.calls.filter((c) => c.path.includes("/comments")).length, 0);
});

test("verdict_post: carries the exact trailer, and is an ISSUE comment — never a review event", async () => {
	const responses = baseResponses();
	responses.set("POST /repos/tibrez/rhesadox/issues/2833/comments", { status: 201, json: { id: 1 } });
	const h = harness({ responses });
	await h.api.start();
	const r = await h.api.call("review_verdict_post", { decision: "REQUEST_CHANGES", body: "4 blocking findings" });
	assert.match(String(r.content[0].text), /verdict REQUEST_CHANGES posted/);
	const vc = h.fake.calls.find((c) => c.method === "POST" && c.path.includes("/issues/2833/comments"));
	assert.match(vc.body.body, /<!-- pr-review: REQUEST_CHANGES @ bf994bb6 -->/);
	// no review EVENT with a verdict state was ever sent
	for (const c of h.fake.calls) {
		if (c.method === "POST" && c.path.endsWith("/reviews")) assert.equal(c.body.event, "COMMENT");
	}
});

test("verdict_post: REFUSED when a verdict trailer already stands at the armed head (#2845 duplicate-currency class)", async () => {
	const responses = baseResponses();
	responses.set("GET /repos/tibrez/rhesadox/issues/2833/comments", {
		status: 200,
		json: [{ id: 1, body: "earlier round text\n\n<!-- pr-review: REQUEST_CHANGES @ bf994bb6 -->" }],
	});
	const h = harness({ responses });
	await h.api.start();
	const r = await h.api.call("review_verdict_post", { decision: "REQUEST_CHANGES", body: "same-head re-post" });
	assert.match(String(r.content[0].text), /REFUSED: a REQUEST_CHANGES verdict already stands at bf994bb6/);
	// no second verdict comment was minted — the POST never happened
	assert.equal(h.fake.calls.filter((c) => c.method === "POST" && c.path.includes("/issues/2833/comments")).length, 0);
	assert.equal(r.details.error, "verdict-standing");
});

test("verdict_post: currency lookup failure is fail-open — a transient host error never costs the round its verdict", async () => {
	const responses = baseResponses();
	responses.set("GET /repos/tibrez/rhesadox/issues/2833/comments", { status: 503, json: { message: "unavailable" } });
	const h = harness({ responses });
	await h.api.start();
	const r = await h.api.call("review_verdict_post", { decision: "APPROVE", body: "clean" });
	assert.match(String(r.content[0].text), /verdict APPROVE posted/);
	assert.equal(h.fake.calls.filter((c) => c.method === "POST" && c.path.includes("/issues/2833/comments")).length, 1);
});

test("verdict_post: a trailer at a DIFFERENT sha does not stand — the round at the new head posts", async () => {
	const responses = baseResponses();
	responses.set("GET /repos/tibrez/rhesadox/issues/2833/comments", {
		status: 200,
		json: [{ id: 1, body: "old round\n\n<!-- pr-review: APPROVE @ deadbeef -->" }],
	});
	const h = harness({ responses });
	await h.api.start();
	const r = await h.api.call("review_verdict_post", { decision: "REQUEST_CHANGES", body: "fresh head" });
	assert.match(String(r.content[0].text), /verdict REQUEST_CHANGES posted/);
});

// ── 2. resume / no-double-post ───────────────────────────────────────────

function toolResult(details: any): any {
	return { type: "message", message: { role: "toolResult", toolName: "review_finding_post", details } };
}

test("resume: branch state reconstructs — a re-round sees the prior thread_ref without re-posting", async () => {
	const branch = [toolResult({ tool: "review-post", posted: [{ thread_ref: "forgejo:x#1:r1:c2", path: "src/a.zig", line: 42, severity: "Blocking", body: "old" }], verdict: undefined })];
	const h = harness({ branch, responses: baseResponses() });
	await h.api.start();
	const list = await h.api.call("review_findings_list", {});
	assert.ok(String(list.content[0].text));
	// state is in the details of the fresh tool call, not lost
	assert.equal(list.details.posted[0].thread_ref, "forgejo:x#1:r1:c2");
});

// ── 3. fail-closed arming ────────────────────────────────────────────────

test("no credential: every tool degrades to the ledger message, nothing throws", async () => {
	const h = harness({ env: { HARMOSTES_FORGEJO_TOKEN: "", HARMOSTES_REVIEW_POST_TOKEN: "" } });
	await h.api.start();
	for (const [name, params] of [
		["review_finding_post", FINDING],
		["review_findings_list", {}],
		["review_thread_reply", { thread_ref: "forgejo:x#1:r1:c2", body: "x" }],
		["review_thread_resolve", { thread_ref: "forgejo:x#1:r1:c2" }],
		["review_verdict_post", { decision: "APPROVE", body: "x" }],
	] as const) {
		const r = await h.api.call(name, params as any);
		assert.match(String(r.content[0].text), /posting not armed/, `${name} must degrade, not throw`);
	}
	assert.equal(h.fake.calls.length, 0, "an unarmed extension must not touch the host");
});

test("no pr-context: same degrade path", async () => {
	const h = harness({ ctx: null });
	await h.api.start();
	const r = await h.api.call("review_finding_post", FINDING);
	assert.match(String(r.content[0].text), /posting not armed/);
});

// ── 4. resolveTarget ─────────────────────────────────────────────────────

test("resolveTarget: forgejo host, github host, unknown host, incomplete context", () => {
	const t1 = resolveTarget({ HARMOSTES_FORGEJO_TOKEN: "t" } as any, CTX as any);
	assert.equal(t1?.forge, "forgejo");
	assert.equal(t1?.apiBase, "https://git.rezus.cloud/api/v1");
	const t2 = resolveTarget({ HARMOSTES_REVIEW_POST_TOKEN: "g" } as any, { ...CTX, host: "github.com" } as any);
	assert.equal(t2?.forge, "github");
	assert.equal(t2?.apiBase, "https://api.github.com");
	assert.equal(resolveTarget({} as any, CTX as any), null, "no credential → null");
	assert.equal(resolveTarget({ HARMOSTES_FORGEJO_TOKEN: "t" } as any, { ...CTX, head_sha: "" } as any), null, "incomplete context → null");
});

// ── 8. the ledger export (#749 Phase 2): review.json is extension-written ─

const fs = require("node:fs");
const path = require("node:path");

function ledgerOf(h: ReturnType<typeof harness>): any {
	return JSON.parse(fs.readFileSync(path.join(h.api.workdir, "review.json"), "utf8"));
}

test("export: finding_post + verdict_post maintain review.json — lean findings, decision, reviewed_sha", async () => {
	const h = harness({ responses: baseResponses() });
	try {
		await h.api.start();
		await h.api.call("review_finding_post", FINDING);
		await h.api.call("review_verdict_post", { decision: "REQUEST_CHANGES", body: "round body" });
		const led = ledgerOf(h);
		assert.equal(led.decision, "REQUEST_CHANGES");
		assert.equal(led.reviewed_sha, "bf994bb6");
		assert.equal(led.findings.length, 1);
		// the lean gate shape: body lives on the host thread, not the ledger
		assert.deepEqual(Object.keys(led.findings[0]).sort(), ["line", "path", "severity", "thread_ref"]);
		assert.equal(led.findings[0].thread_ref, "forgejo:tibrez/rhesadox#2833:r1407:c99");
	} finally {
		h.api.done();
	}
});

test("export: foreign keys survive (todos, coverage) — read-modify-write, never a clobber", async () => {
	const h = harness({ responses: baseResponses() });
	fs.writeFileSync(path.join(h.api.workdir, "review.json"), JSON.stringify({
		decision: "COMMENT",
		todos: [{ path: "x.zig", disposition: "architecture-carry" }],
		coverage: { covered: [{ path: "x.zig" }], frontier: [] },
	}));
	try {
		await h.api.start();
		await h.api.call("review_finding_post", FINDING);
		const led = ledgerOf(h);
		assert.equal(led.findings.length, 1);
		assert.deepEqual(led.todos, [{ path: "x.zig", disposition: "architecture-carry" }]);
		assert.deepEqual(Object.keys(led.coverage), ["covered", "frontier"]);
		// decision is tool-owned only once a verdict is posted — the
		// pre-existing value stands until review_verdict_post
		assert.equal(led.decision, "COMMENT");
		await h.api.call("review_verdict_post", { decision: "APPROVE", body: "v" });
		assert.equal(ledgerOf(h).decision, "APPROVE");
	} finally {
		h.api.done();
	}
});

test("export: failed post writes nothing; unarmed start never touches a hand-written ledger", async () => {
	const responses = new Map([["POST /repos/tibrez/rhesadox/pulls/2833/reviews", { status: 403, json: { message: "forbidden" } }]]);
	const h = harness({ responses });
	const handLedger = JSON.stringify({ decision: "REQUEST_CHANGES", findings: [{ path: "hand.zig" }] });
	fs.writeFileSync(path.join(h.api.workdir, "review.json"), handLedger);
	try {
		await h.api.start(); // armed but empty state → no export
		assert.equal(fs.readFileSync(path.join(h.api.workdir, "review.json"), "utf8"), handLedger);
		await h.api.call("review_finding_post", FINDING); // 403 → error result
		assert.equal(fs.readFileSync(path.join(h.api.workdir, "review.json"), "utf8"), handLedger);
	} finally {
		h.api.done();
	}
	// unarmed: session_start with reconstructed state must not clobber
	const h2 = harness({ env: { HARMOSTES_FORGEJO_TOKEN: "" }, responses: baseResponses() });
	fs.writeFileSync(path.join(h2.api.workdir, "review.json"), handLedger);
	try {
		await h2.api.start();
		assert.equal(fs.readFileSync(path.join(h2.api.workdir, "review.json"), "utf8"), handLedger);
	} finally {
		h2.api.done();
	}
});

test("export: resume re-sync repairs a missing projection without re-posting", async () => {
	const branch = [{
		type: "message",
		message: {
			role: "toolResult",
			toolName: "review_finding_post",
			details: {
				tool: "review-post",
				posted: [{ thread_ref: "forgejo:tibrez/rhesadox#2833:r1400:c1", path: "src/old.zig", line: 7, severity: "Blocking", body: "old" }],
			},
		},
	}];
	const h = harness({ branch, responses: baseResponses() });
	try {
		await h.api.start();
		const led = ledgerOf(h); // the projection exists again — no host call made
		assert.equal(led.findings.length, 1);
		assert.equal(led.findings[0].thread_ref, "forgejo:tibrez/rhesadox#2833:r1400:c1");
		assert.ok(!h.fake.calls.some((c) => c.method === "POST" && c.path.endsWith("/reviews")));
	} finally {
		h.api.done();
	}
});

// ── 9. the moved-head guard (#749 Phase 3): the verdict is bound to the head ─

test("verdict_post: the moved-head guard refuses — nothing posted, nothing recorded", async () => {
	const responses = baseResponses();
	responses.set("GET /repos/tibrez/rhesadox/pulls/2833", { status: 200, json: { head: { sha: "9999999newhead" } } });
	const h = harness({ responses });
	try {
		await h.api.start();
		const r = await h.api.call("review_verdict_post", { decision: "REQUEST_CHANGES", body: "b" });
		assert.match(String(r.details?.error), /head-moved/);
		assert.ok(!h.fake.calls.some((c) => c.method === "POST" && c.path.includes("/comments")));
		assert.ok(!fs.existsSync(path.join(h.api.workdir, "review.json")));
	} finally {
		h.api.done();
	}
});

// ── 10. the converge tools (#749 Phase 3): research, coverage, carry ─────

test("research_publish: frontmatter + atomic write + idempotent name; empty notes = no-op", async () => {
	const store = fs.mkdtempSync(path.join(require("node:os").tmpdir(), "rp-store-"));
	const h = harness({ responses: baseResponses(), env: { HARMOSTES_SOL_PI_STORE: store } });
	process.env.HARMOSTES_SOL_PI_STORE = store;
	try {
		await h.api.start();
		// empty/no-file → clean no-op
		const r0 = await h.api.call("review_research_publish", {});
		assert.match(String(r0.content[0].text), /nothing durable/);
		// real notes → store file with frontmatter, named <pr>-<sha8>
		fs.writeFileSync(path.join(h.api.workdir, "research-out.md"), "the rig tool caches by sha");
		const r = await h.api.call("review_research_publish", {});
		assert.match(String(r.content[0].text), /research published/);
		const files = fs.readdirSync(path.join(store, "_shared/research"));
		assert.deepEqual(files, ["2833-bf994bb6.md"]);
		const body = fs.readFileSync(path.join(store, "_shared/research", files[0]), "utf8");
		assert.match(body, /^---\nrepo: /);
		assert.match(body, /head_sha: bf994bb6/);
	} finally {
		process.env.HARMOSTES_SOL_PI_STORE = "";
		h.api.done();
	}
});

test("coverage_publish: merges into the per-PR ledger; APPROVE+frontier is REFUSED", async () => {
	const store = fs.mkdtempSync(path.join(require("node:os").tmpdir(), "rp-store-"));
	const responses = new Map([
		["POST /repos/tibrez/rhesadox/pulls/2833/reviews", OK_REVIEW],
		["GET /repos/tibrez/rhesadox/pulls/2833/reviews/1407/comments", OK_COMMENT],
		["POST /repos/tibrez/rhesadox/issues/2833/comments", { status: 200, json: { id: 5001 } }],
		// the moved-head guard reads the live head on every verdict
		["GET /repos/tibrez/rhesadox/pulls/2833", { status: 200, json: { head: { sha: "bf994bb6" } } }],
		// the verdict-currency lookup reads the conversation (no trailer = no standing verdict)
		["GET /repos/tibrez/rhesadox/issues/2833/comments", { status: 200, json: [] }],
	]);
	const h = harness({ responses, env: { HARMOSTES_SOL_PI_STORE: store } });
	process.env.HARMOSTES_SOL_PI_STORE = store;
	try {
		await h.api.start();
		await h.api.call("review_finding_post", FINDING);
		// the refusal: APPROVE + frontier
		await h.api.call("review_verdict_post", { decision: "APPROVE", body: "v" });
		const r = await h.api.call("review_coverage_publish", { covered: [{ path: "a.zig" }], frontier: [{ path: "b.zig", reason: "not examined" }] });
		assert.match(String(r.content[0].text), /REFUSED/);
		assert.ok(!fs.existsSync(path.join(store, "coverage/2833.json")));
		// honest declaration merges: round 1, path-keyed, foreign keys kept
		const led = ledgerOf(h);
		assert.equal(led.decision, "APPROVE");
		const r2 = await h.api.call("review_coverage_publish", { covered: [{ path: "a.zig", note: "full" }], frontier: [] });
		assert.match(String(r2.content[0].text), /coverage merged: round 1/);
		const stored = JSON.parse(fs.readFileSync(path.join(store, "coverage/2833.json"), "utf8"));
		assert.equal(stored.rounds, 1);
		assert.deepEqual(stored.covered.map((c: any) => c.path), ["a.zig"]);
	} finally {
		process.env.HARMOSTES_SOL_PI_STORE = "";
		h.api.done();
	}
});

test("carry_file: mint-gate + dedup + reply/resolve; resolve failure keeps the thread open", async () => {
	const responses = new Map([
		["GET /repos/tibrez/rhesadox/issues", { status: 200, json: [{ number: 7, title: "architecture: fix the classifier tokenization drift" }] }],
		["GET /repos/tibrez/rhesadox/milestones", { status: 200, json: [{ id: 3, title: "Architecture & Design improvements" }] }],
		["POST /repos/tibrez/rhesadox/issues", { status: 201, json: { number: 42 } }],
		["POST /repos/tibrez/rhesadox/pulls/2833/comments", { status: 201, json: { id: 6001 } }],
		["PUT /repos/tibrez/rhesadox/pulls/2833/comments/60333/resolutions", { status: 201, json: {} }],
	]);
	const h = harness({ responses, env: { HARMOSTES_SOL_PI_STORE: "" } });
	fs.writeFileSync(path.join(h.api.workdir, "review.json"), JSON.stringify({
		todos: [
			{ path: "a.zig", body: "the classifier diffs the base TIP, not the merge base", disposition: "architecture-carry", thread_ref: "forgejo:tibrez/rhesadox#2833:r1407:c60333" },
			{ path: "b.zig", body: "severe: the gate can pass on a truncated scan — mint this one", disposition: "architecture-carry" },
			{ path: "c.zig", body: "arch-sync page wording drift (generated artifact)", disposition: "architecture-carry" },
		],
	}));
	try {
		await h.api.start();
		const r = await h.api.call("review_carry_file", {});
		const txt = String(r.content[0].text);
		assert.match(txt, /3 entr/);
		// 1: deduped against issue #7 (tokenization/bigram jaccard), thread replied+resolved
		// 2: MINT_WORTHY (severe) → minted as #42 with the architecture milestone
		// 3: GENERATED → review-record carry, no mint, no thread (none posted)
		const mintCall = h.fake.calls.find((c) => c.method === "POST" && c.path === "/repos/tibrez/rhesadox/issues");
		assert.ok(mintCall, "the severe/structural carry mints");
		assert.equal(mintCall.body.milestone, 3);
		assert.match(String(mintCall.body.body), /severe: the gate can pass/);
		const resolveCall = h.fake.calls.find((c) => c.method === "PUT" && c.path.includes("/resolutions"));
		assert.ok(resolveCall, "the carried thread resolves");
		const d = r.details.carry;
		assert.equal(d.minted.length, 1);
		assert.equal(d.resolved.length, 1);
		assert.equal(d.failed.length, 0);
	} finally {
		h.api.done();
	}
});
