/**
 * review-post — pi extension: the reviewer's posting machinery (#749).
 *
 * One contract, every host: the agent posts a native anchored thread the
 * moment a finding is PROVEN; the verdict COMMENT (trailer-carrying) is
 * also agent-side; the verdict review EVENT (REQUEST_CHANGES/APPROVED)
 * and label consumption are the gate sweep's alone — this extension has
 * NO verdict-event path on any host (#680 by code: the agent process
 * must not be able to emit binding verdicts, whatever its token could
 * theoretically do).
 *
 * Posting state lives in tool-result details (the pillar-todo contract):
 * reconstructed from the session branch on resume, so a re-round never
 * double-posts and always knows what landed. thread_ref is accepted ONLY
 * from host 2xx responses (#727 doctrine) — a failure returns an error
 * result, never a fabricated ref.
 *
 * Credentials (Runtime-Environment-Reference): HARMOSTES_FORGEJO_TOKEN
 * (the fleet-wide Forgejo credential) and HARMOSTES_REVIEW_POST_TOKEN
 * (github.com half). Absent token → the tools are inert (typed error,
 * "posting not armed") and the round degrades to the ledger path — a
 * supported mode, never a failure.
 *
 * Replaces the agent-facing half of plugins/post-review/post-review.sh
 * (#429's "the agent never reliably self-posts" premise dies here: the
 * agent never reliably self-posted because it had bash and a 403-able
 * channel — not because posting is not a tool).
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { readFileSync, writeFileSync, renameSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { Type } from "typebox"; // pi aliases typebox for extensions — the rig-query form

interface PrContext {
	host?: string;
	repo?: string; // "owner/name"
	number?: number;
	head_sha?: string;
}

interface PostedFinding {
	thread_ref: string; // host-issued only (2xx)
	path: string;
	line: number;
	severity: string;
	body: string;
}

interface ReviewPostDetails {
	tool: string;
	posted: PostedFinding[];
	verdict?: string;
	error?: string;
	exportError?: string; // ledger-export failure — non-fatal, repaired on session_start
}

// The one shape every finding_post/thread_ref answer takes.
const Str = Type.String;
const Num = Type.Number;

const FindingParams = Type.Object({
	path: Str({ description: "File path, repo-relative" }),
	line: Num({ description: "Line number (file-side/new line)" }),
	severity: Type.Unsafe<{ type: "string"; enum: string[] }>({
		type: "string",
		enum: ["Blocking", "Non-blocking"],
	}),
	body: Str({ description: "The finding: what is wrong, the evidence, the fix direction" }),
});

const RefParams = Type.Object({
	thread_ref: Str({ description: "Thread reference as returned by finding_post" }),
	body: Type.Optional(Str({ description: "Reply text (thread_reply)" })),
});

const VerdictParams = Type.Object({
	decision: Type.Unsafe<{ type: "string"; enum: string[] }>({
		type: "string",
		enum: ["APPROVE", "REQUEST_CHANGES"],
	}),
	body: Str({ description: "The verdict narrative — summarizes the posted threads" }),
});

// ── host abstraction ─────────────────────────────────────────────────────

interface HostTarget {
	forge: "forgejo" | "github";
	base: string; // https://host
	apiBase: string; // https://host/api/v1 | https://api.github.com
	owner: string;
	repo: string;
	number: number;
	sha: string;
	token: string;
}

/**
 * resolveTarget: the single place that decides where and as whom we
 * post. Host + repo + number + head come from pr-context.json (the
 * workspace writes it from the trigger envelope — never from the
 * model's claims). The credential is an env fact (fail-closed: absent
 * → null → tools inert).
 */
export function resolveTarget(env: NodeJS.ProcessEnv, ctx: PrContext | null): HostTarget | null {
	const repo = ctx?.repo ?? "";
	const number = ctx?.number ?? 0;
	const sha = ctx?.head_sha ?? "";
	if (!repo.includes("/") || !number || !sha) return null;
	const [owner, name] = repo.split("/");
	const host = (ctx?.host ?? "").toLowerCase();
	if (host.includes("github.com")) {
		const token = env.HARMOSTES_REVIEW_POST_TOKEN;
		if (!token) return null;
		return { forge: "github", base: "https://github.com", apiBase: "https://api.github.com", owner, repo: name, number, sha, token };
	}
	// every /api/v1 forge we run is Forgejo-class (git.rezus.cloud, codeberg)
	const token = env.HARMOSTES_FORGEJO_TOKEN;
	if (!token) return null;
	const base = host && !host.includes("github.com") ? `https://${host}` : "https://git.rezus.cloud";
	return { forge: "forgejo", base, apiBase: `${base}/api/v1`, owner, repo: name, number, sha, token };
}

interface HostClient {
	postFinding(f: { path: string; line: number; severity: string; body: string }): Promise<string>;
	listFindings(): Promise<{ thread_ref: string; path: string; body: string }[]>;
	reply(ref: string, body: string): Promise<void>;
	resolve(ref: string): Promise<void>;
	postVerdictComment(decision: string, body: string): Promise<void>;
	// Verdict currency check (#567 mirror, tool-side): does a verdict
	// trailer comment already stand at the armed head? True = a round has
	// already spoken at this SHA — a second verdict would be a duplicate
	// currency the sweep's memo never asked for.
	verdictStandingAt(): Promise<string | null>;
	// #749 Phase 3: the carry tool's tracker verbs + the moved-head guard.
	listIssues(): Promise<{ number: number; title: string }[]>;
	createIssue(title: string, body: string, milestone?: number | string): Promise<number>;
	listMilestones(): Promise<{ id: number | string; title: string }[]>;
	headSha(): Promise<string>;
}

/** Minimal JSON fetch with the one header set every route needs. */
async function call(t: HostTarget, method: string, path: string, body?: unknown): Promise<{ status: number; json: any }> {
	const res = await fetch(`${t.apiBase}${path}`, {
		method,
		headers: {
			authorization: `token ${t.token}`,
			"content-type": "application/json",
			accept: "application/json",
		},
		body: body === undefined ? undefined : JSON.stringify(body),
	});
	let json: any = undefined;
	try {
		json = await res.json();
	} catch {
		// 204s and empty bodies are legal
	}
	return { status: res.status, json };
}

function verdictTrailerAt(comments: { body?: string }[], sha: string): string | null {
	for (const c of comments) {
		const m = /<!-- pr-review: (\w+) @ ([0-9a-f]+)/.exec(String(c.body ?? ""));
		// prefix match: the trailer carries the full sha; the armed sha may
		// be abbreviated by the trigger envelope
		if (m && sha.startsWith(m[2])) return m[1];
	}
	return null;
}

function forgejoClient(t: HostTarget): HostClient {
	const base = `/repos/${t.owner}/${t.repo}/pulls/${t.number}`;
	return {
		// One review event carrying ONE anchored comment — event COMMENT
		// (never APPROVED/REQUEST_CHANGES: verdict events are the sweep's).
		async postFinding(f) {
			const { status, json } = await call(t, "POST", `${base}/reviews`, {
				event: "COMMENT",
				commit_id: t.sha,
				body: f.severity,
				comments: [{ path: f.path, new_position: f.line, body: f.body }],
			});
			if (status !== 200 && status !== 201) throw new Error(`forgejo post ${status}: ${JSON.stringify(json)?.slice(0, 200)}`);
			// thread_ref = review:comment — fetch the review's comments for the id
			const rid = json?.id;
			const { status: s2, json: comments } = await call(t, "GET", `${base}/reviews/${rid}/comments`);
			if (s2 !== 200 || !Array.isArray(comments) || comments.length === 0) throw new Error(`forgejo comment-lookup ${s2}`);
			return `forgejo:${t.owner}/${t.repo}#${t.number}:r${rid}:c${comments[comments.length - 1].id}`;
		},
		async listFindings() {
			const out: { thread_ref: string; path: string; body: string }[] = [];
			const { status, json: reviews } = await call(t, "GET", `${base}/reviews?limit=50`);
			if (status !== 200 || !Array.isArray(reviews)) throw new Error(`forgejo list ${status}`);
			for (const r of reviews) {
				const { status: s2, json: comments } = await call(t, "GET", `${base}/reviews/${r.id}/comments`);
				if (s2 !== 200 || !Array.isArray(comments)) continue;
				for (const c of comments) {
					out.push({ thread_ref: `forgejo:${t.owner}/${t.repo}#${t.number}:r${r.id}:c${c.id}`, path: c.path ?? "", body: c.body ?? "" });
				}
			}
			return out;
		},
		async reply(ref, body) {
			const cid = Number(ref.split(":c")[1]);
			const { status, json } = await call(t, "POST", `${base}/comments/${cid}/replies`, { body });
			if (status !== 200 && status !== 201) throw new Error(`forgejo reply ${status}: ${JSON.stringify(json)?.slice(0, 120)}`);
		},
		async resolve(ref) {
			const cid = Number(ref.split(":c")[1]);
			const { status } = await call(t, "PUT", `${base}/comments/${cid}/resolutions`);
			if (status !== 200 && status !== 201 && status !== 204) throw new Error(`forgejo resolve ${status}`);
		},
		// The trailer-carrying conversation comment — the merge currency.
		// NOT a review event; branch protection never sees this as a verdict.
		async postVerdictComment(decision, body) {
			const text = `${body}\n\n<!-- pr-review: ${decision} @ ${t.sha} -->`;
			const { status, json } = await call(t, "POST", `/repos/${t.owner}/${t.repo}/issues/${t.number}/comments`, { body: text });
			if (status !== 200 && status !== 201) throw new Error(`forgejo verdict ${status}: ${JSON.stringify(json)?.slice(0, 120)}`);
		},
		async verdictStandingAt() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/issues/${t.number}/comments?limit=50`);
			if (status !== 200 || !Array.isArray(json)) throw new Error(`forgejo verdict-lookup ${status}`);
			return verdictTrailerAt(json, t.sha);
		},
		// ── the carry tool's tracker verbs + the moved-head guard (#749 Phase 3)
		async listIssues() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/issues?state=all&type=issues&limit=100`);
			if (status !== 200) throw new Error(`list issues failed: HTTP ${status}`);
			return (Array.isArray(json) ? json : []).filter((i: any) => !i.pull_request).map((i: any) => ({ number: i.number, title: String(i.title ?? "") }));
		},
		async createIssue(title: string, body: string, milestone?: number | string) {
			const payload: any = { title, body };
			if (milestone !== undefined) payload.milestone = milestone; // forgejo takes the milestone ID
			const { status, json } = await call(t, "POST", `/repos/${t.owner}/${t.repo}/issues`, payload);
			if (status < 200 || status >= 300) throw new Error(`issue mint failed: HTTP ${status}`);
			return Number(json.number);
		},
		async listMilestones() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/milestones?state=open`);
			if (status !== 200) return [];
			return (Array.isArray(json) ? json : []).map((m: any) => ({ id: m.id, title: String(m.title ?? "") }));
		},
		async headSha() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/pulls/${t.number}`);
			if (status !== 200) throw new Error(`head fetch failed: HTTP ${status}`);
			return String(json?.head?.sha ?? "");
		},
	};
}

function githubClient(t: HostTarget): HostClient {
	const base = `/repos/${t.owner}/${t.repo}/pulls/${t.number}`;
	return {
		// Review-event COMMENT with one anchored comment (same shape as
		// forgejo; GitHub calls the anchored comment a "review comment").
		async postFinding(f) {
			const { status, json } = await call(t, "POST", `${base}/reviews`, {
				event: "COMMENT",
				commit_id: t.sha,
				body: f.severity,
				comments: [{ path: f.path, line: f.line, body: f.body }],
			});
			if (status !== 200 && status !== 201) throw new Error(`github post ${status}: ${JSON.stringify(json)?.slice(0, 200)}`);
			const rid = json?.id;
			const { status: s2, json: full } = await call(t, "GET", `${base}/reviews/${rid}/comments`);
			if (s2 !== 200 || !Array.isArray(full) || full.length === 0) throw new Error(`github comment-lookup ${s2}`);
			return `github:${t.owner}/${t.repo}#${t.number}:r${rid}:c${full[full.length - 1].id}`;
		},
		async listFindings() {
			const out: { thread_ref: string; path: string; body: string }[] = [];
			const { status, json: comments } = await call(t, "GET", `${base}/comments?per_page=100`);
			if (status !== 200 || !Array.isArray(comments)) throw new Error(`github list ${status}`);
			for (const c of comments) {
				out.push({ thread_ref: `github:${t.owner}/${t.repo}#${t.number}:r${c.pull_request_review_id}:c${c.id}`, path: c.path ?? "", body: c.body ?? "" });
			}
			return out;
		},
		async reply(ref, body) {
			const cid = Number(ref.split(":c")[1]);
			const { status, json } = await call(t, "POST", `${base}/comments/${cid}/replies`, { body });
			if (status !== 200 && status !== 201) throw new Error(`github reply ${status}: ${JSON.stringify(json)?.slice(0, 120)}`);
		},
		async resolve(ref) {
			// GitHub resolves via the thread id (different graph than the
			// comment id) — resolve through the comment's thread link.
			const cid = Number(ref.split(":c")[1]);
			const { status: s1, json: c } = await call(t, "GET", `${base}/comments/${cid}`);
			const threadId = c?.pull_request_review_thread?.id ?? c?.thread_id;
			if (s1 !== 200 || !threadId) throw new Error(`github resolve-lookup ${s1}`);
			const { status } = await call(t, "PATCH", `/pulls/${t.owner}/${t.repo}/threads/${threadId}`, { resolved: true });
			if (status !== 200) throw new Error(`github resolve ${status}`);
		},
		async postVerdictComment(decision, body) {
			const text = `${body}\n\n<!-- pr-review: ${decision} @ ${t.sha} -->`;
			const { status, json } = await call(t, "POST", `/repos/${t.owner}/${t.repo}/issues/${t.number}/comments`, { body: text });
			if (status !== 200 && status !== 201) throw new Error(`github verdict ${status}: ${JSON.stringify(json)?.slice(0, 120)}`);
		},
		async verdictStandingAt() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/issues/${t.number}/comments?per_page=50`);
			if (status !== 200 || !Array.isArray(json)) throw new Error(`github verdict-lookup ${status}`);
			return verdictTrailerAt(json, t.sha);
		},
		// ── the carry tool's tracker verbs + the moved-head guard (#749 Phase 3)
		async listIssues() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/issues?state=all&per_page=100&sort=created&direction=desc`);
			if (status !== 200) throw new Error(`list issues failed: HTTP ${status}`);
			return (Array.isArray(json) ? json : []).filter((i: any) => !i.pull_request).map((i: any) => ({ number: i.number, title: String(i.title ?? "") }));
		},
		async createIssue(title: string, body: string, milestone?: number | string) {
			const payload: any = { title, body };
			if (milestone !== undefined) payload.milestone = milestone; // GitHub takes the milestone NUMBER
			const { status, json } = await call(t, "POST", `/repos/${t.owner}/${t.repo}/issues`, payload);
			if (status < 200 || status >= 300) throw new Error(`issue mint failed: HTTP ${status}`);
			return Number(json.number);
		},
		async listMilestones() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/milestones?state=open`);
			if (status !== 200) return [];
			return (Array.isArray(json) ? json : []).map((m: any) => ({ id: m.id, title: String(m.title ?? "") }));
		},
		async headSha() {
			const { status, json } = await call(t, "GET", `/repos/${t.owner}/${t.repo}/pulls/${t.number}`);
			if (status !== 200) throw new Error(`head fetch failed: HTTP ${status}`);
			return String(json?.head?.sha ?? "");
		},
	};
}

function clientFor(t: HostTarget): HostClient {
	return t.forge === "github" ? githubClient(t) : forgejoClient(t);
}

function readPrContext(workdir: string): PrContext | null {
	try {
		return JSON.parse(readFileSync(`${workdir}/pr-context.json`, "utf8")) as PrContext;
	} catch {
		return null;
	}
}

export default function (pi: ExtensionAPI) {
	let posted: PostedFinding[] = [];
	let verdict: string | undefined;

	const reconstruct = (ctx: ExtensionContext) => {
		posted = [];
		verdict = undefined;
		for (const entry of ctx.sessionManager.getBranch()) {
			if (entry.type !== "message") continue;
			const msg = entry.message as { role?: string; toolName?: string; details?: ReviewPostDetails } | undefined;
			if (msg?.role !== "toolResult") continue;
			if (!msg.toolName?.startsWith("review_")) continue;
			const d = msg.details;
			if (!d) continue;
			if (d.posted) posted = d.posted;
			if (d.verdict) verdict = d.verdict;
		}
	};

	const target = (env: NodeJS.ProcessEnv): { t: HostTarget; c: HostClient } | { err: string } => {
		const t = resolveTarget(env, readPrContext(env.HARMOSTES_WORKDIR ?? "/workspace"));
		if (!t) return { err: "posting not armed (no credential or no pr-context) — record the finding in the ledger; the pipeline publishes it" };
		return { t, c: clientFor(t) };
	};

	const details = (): ReviewPostDetails => ({ tool: "review-post", posted, verdict });

	// ── the ledger export (#749 Phase 2): review.json is the extension-
	// written export ────────────────────────────────────────────────────
	// While armed, the TOOLS own findings[] and decision in
	// $HARMOSTES_WORKDIR/review.json; the agent stops hand-writing JSON
	// for those fields (unarmed rounds hand-write the whole ledger — the
	// supported inert mode, and this function is never called there).
	// Foreign keys (todos[], coverage, anything else) are preserved
	// verbatim: read-modify-write, atomic tmp+rename. Write failure is
	// NON-FATAL: the thread is on the host (the gate counts threads
	// natively) and the session_start re-sync repairs the projection on
	// resume. Never exports an empty state — an unarmed hand-written
	// ledger must never be clobbered.
	const exportLedger = (sha: string): { exported: boolean; error?: string } => {
		if (!sha || (posted.length === 0 && !verdict)) return { exported: false };
		const dir = process.env.HARMOSTES_WORKDIR ?? "/workspace";
		const p = `${dir}/review.json`;
		try {
			let ledger: Record<string, unknown> = {};
			try {
				ledger = JSON.parse(readFileSync(p, "utf8")) as Record<string, unknown>;
			} catch {
				/* new ledger — start lean; a corrupt one is replaced, not merged */
			}
			ledger.reviewed_sha = sha;
			if (verdict) ledger.decision = verdict;
			// the lean gate shape (#680/#749): the body lives on the host
			// thread; findings[] is REPLACED by tool state, not merged.
			ledger.findings = posted.map((f) => ({ path: f.path, line: f.line, severity: f.severity, thread_ref: f.thread_ref }));
			const tmp = `${p}.tmp`;
			writeFileSync(tmp, JSON.stringify(ledger, null, 2) + "\n");
			renameSync(tmp, p);
			return { exported: true };
		} catch (e) {
			return { exported: false, error: e instanceof Error ? e.message : String(e) };
		}
	};

	pi.on("session_start", async (_event, ctx) => {
		reconstruct(ctx);
		// re-sync the export from reconstructed state (a crashed round's
		// projection catches up without a re-post; empty state exports
		// nothing, so unarmed hand-written ledgers are safe)
		const rt = resolveTarget(process.env, readPrContext(process.env.HARMOSTES_WORKDIR ?? "/workspace"));
		if (rt) exportLedger(rt.sha);
	});

	// ── finding_post — native anchored thread at proof time ────────────────
	pi.registerTool({
		name: "review_finding_post",
		label: "Post finding",
		description:
			"Post a verified finding as a native anchored thread, NOW (progressive posting). Call it the moment a finding's proof bar is met — do not batch. Returns the thread_ref (from the host only). Never call for unproven hunches.",
		parameters: FindingParams,
		execute: async (_id, params) => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			try {
				const ref = await t.c.postFinding(params);
				posted = posted.filter((p) => !(p.path === params.path && p.line === params.line));
				posted.push({ thread_ref: ref, path: params.path, line: params.line, severity: params.severity, body: params.body });
				const ex = exportLedger(t.t.sha);
				return {
					content: [{ type: "text", text: `posted ${params.severity} thread at ${params.path}:${params.line} — thread_ref ${ref}` + (ex.error ? ` (ledger export failed: ${ex.error})` : "") }],
					details: { ...details(), exportError: ex.error },
				};
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				// A failure is terminal for THIS call — never a fabricated ref,
				// never a retry storm (the env is frozen at pod start; a 401/403
				// will not heal by retrying).
				return { content: [{ type: "text", text: `post failed: ${msg} — record in the ledger; do not retry without new facts` }], details: { ...details(), error: msg } };
			}
		},
	});

	// ── findings_list — what the HOST says is landed ───────────────────────
	pi.registerTool({
		name: "review_findings_list",
		label: "List landed threads",
		description: "List the review threads the host currently holds for this PR (authoritative landed state — use to dedupe before posting on a re-round).",
		parameters: Type.Object({}),
		execute: async () => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			try {
				const list = await t.c.listFindings();
				return { content: [{ type: "text", text: list.map((f) => `${f.thread_ref} ${f.path} :: ${f.body.slice(0, 80)}`).join("\n") || "(no threads)" }], details: details() };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `list failed: ${msg}` }], details: { ...details(), error: msg } };
			}
		},
	});

	// ── thread_reply / thread_resolve — the close-out pair ─────────────────
	pi.registerTool({
		name: "review_thread_reply",
		label: "Reply to thread",
		description: "Reply on an existing review thread (e.g. the fix-verification note). A reply never closes a thread.",
		parameters: RefParams,
		execute: async (_id, params) => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			try {
				await t.c.reply(params.thread_ref, params.body ?? "");
				return { content: [{ type: "text", text: `replied on ${params.thread_ref}` }], details: details() };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `reply failed: ${msg}` }], details: { ...details(), error: msg } };
			}
		},
	});

	pi.registerTool({
		name: "review_thread_resolve",
		label: "Resolve thread",
		description: "Resolve (close) a review thread — the close-out. Only for threads whose fix you have VERIFIED at the reviewed SHA.",
		parameters: Type.Object({ thread_ref: RefParams.properties.thread_ref }),
		execute: async (_id, params) => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			try {
				await t.c.resolve(params.thread_ref);
				return { content: [{ type: "text", text: `resolved ${params.thread_ref}` }], details: details() };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `resolve failed: ${msg}` }], details: { ...details(), error: msg } };
			}
		},
	});

	// ── verdict_post — the trailer comment; the EVENT is the sweep's ───────
	pi.registerTool({
		name: "review_verdict_post",
		label: "Post verdict",
		description:
			"Post the round's verdict comment (ends with the pr-review trailer the platform consumes). POST ONLY AFTER every proven finding is posted and the ledger is written. This posts a COMMENT — the binding review event is posted by the pipeline, not by you.",
		parameters: VerdictParams,
		execute: async (_id, params) => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			try {
				// Moved-head guard (ADR-0006, agent-side half): the verdict is
				// only valid at the exact head this session armed at. A moved
				// head refuses IN-TOOL — never posts, never writes the ledger;
				// the synchronize wake re-arms a fresh round at the new head.
				const live = await t.c.headSha();
				if (live && !live.startsWith(t.t.sha)) {
					return { content: [{ type: "text", text: `verdict REFUSED: the PR head moved (${t.t.sha.slice(0, 8)} → ${live.slice(0, 8)}) — review the new head; do not retry` }], details: { ...details(), error: "head-moved" } };
				}
				// Verdict currency (#567 mirror): one verdict per head. A
				// re-post at a head whose trailer already stands (the #2845
				// class: "unchanged from the previous round — same head")
				// mints duplicate merge currency the sweep's memo never asked
				// for. Lookup failure is fail-open (transient host noise must
				// not cost the round its verdict) — the sweep memo still
				// guards the gate side.
				let standing: string | null = null;
				try {
					standing = await t.c.verdictStandingAt();
				} catch {
					// unavailable: proceed (currency stays the sweep's to enforce)
				}
				if (standing) {
					return { content: [{ type: "text", text: `verdict REFUSED: a ${standing} verdict already stands at ${t.t.sha.slice(0, 8)} — the round has spoken (#567 currency); record any new evidence as findings or push a fix commit to re-review. Do not re-post.` }], details: { ...details(), error: "verdict-standing" } };
				}
				await t.c.postVerdictComment(params.decision, params.body);
				verdict = params.decision;
				const ex = exportLedger(t.t.sha);
				return { content: [{ type: "text", text: `verdict ${params.decision} posted at ${t.t.sha}` + (ex.error ? ` (ledger export failed: ${ex.error})` : "") }], details: { ...details(), exportError: ex.error } };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `verdict post failed: ${msg} — record the decision in the ledger; the pipeline publishes it` }], details: { ...details(), error: msg } };
			}
		},
	});

	// ── research_publish — the agent's orientation notes → the shared store ─
	pi.registerTool({
		name: "review_research_publish",
		label: "Publish research notes",
		description:
			"Publish this round's durable orientation notes (research-out.md in the workspace) to the shared research store with provenance frontmatter. Call once at round end, after the verdict. NON-FATAL by design: losing notes costs the next round its head start, never this round its verdict.",
		parameters: Type.Object({}),
		execute: async () => {
			const dir = process.env.HARMOSTES_WORKDIR ?? "/workspace";
			const store = process.env.HARMOSTES_SOL_PI_STORE ?? "";
			const out = `${dir}/research-out.md`;
			let notes = "";
			try {
				notes = readFileSync(out, "utf8").trim();
			} catch {
				return { content: [{ type: "text", text: "no research-out.md — nothing durable to publish (not an error)" }], details: details() };
			}
			if (!notes) return { content: [{ type: "text", text: "research-out.md is empty — nothing to publish" }], details: details() };
			if (!store) return { content: [{ type: "text", text: "no research store configured — notes kept in the workspace only" }], details: { ...details(), exportError: "no-store" } };
			try {
				const leaf = `${store}/_shared/research`;
				mkdirSync(leaf, { recursive: true });
				const ctxFile = readPrContext(dir);
				const sha = (ctxFile.head_sha ?? "nosha").slice(0, 8);
				const pr = ctxFile.number ?? "0";
				const front = `---\nrepo: ${process.env.HARMOSTES_TRIGGER_REPO ?? "unknown"}\nworkflow: ${process.env.HARMOSTES_WORKFLOW ?? "unknown"}\npr: "${pr}"\nhead_sha: ${ctxFile.head_sha ?? ""}\nwritten_at: ${new Date().toISOString().replace(/\.\d{3}Z$/, "Z")}\n---\n\n`;
				const target = `${leaf}/${pr}-${sha}.md`;
				const tmp = `${leaf}/.research-${process.pid}-${Date.now()}`;
				writeFileSync(tmp, front + notes + "\n");
				renameSync(tmp, target);
				return { content: [{ type: "text", text: `research published: ${target}` }], details: details() };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `research publish failed (non-fatal): ${msg}` }], details: { ...details(), exportError: msg } };
			}
		},
	});

	// ── coverage_publish — the round's closing statement, merged to the store ─
	pi.registerTool({
		name: "review_coverage_publish",
		label: "Publish coverage",
		description:
			"Merge this round's coverage block (covered = files FULLY reviewed at the reviewed SHA; frontier = files seen but not examined) into the per-PR coverage ledger. Call once at round end, after the verdict. An APPROVE with a declared frontier is REFUSED — fix the verdict first.",
		parameters: Type.Object({
			covered: Type.Array(Type.Object({ path: Type.String(), note: Type.Optional(Type.String()) })),
			frontier: Type.Array(Type.Object({ path: Type.String(), reason: Type.Optional(Type.String()) })),
		}),
		execute: async (_id, params) => {
			const dir = process.env.HARMOSTES_WORKDIR ?? "/workspace";
			const store = process.env.HARMOSTES_SOL_PI_STORE ?? "";
			let ledger: any = {};
			try {
				ledger = JSON.parse(readFileSync(`${dir}/review.json`, "utf8"));
			} catch {
				return { content: [{ type: "text", text: "no ledger to publish coverage against — post findings/the verdict first" }], details: { ...details(), error: "no-ledger" } };
			}
			if (String(ledger.decision ?? "").toUpperCase() === "APPROVE" && params.frontier.length > 0) {
				return { content: [{ type: "text", text: "REFUSED: an APPROVE with a declared frontier is a coverage lie — declare REQUEST_CHANGES (or drop frontier entries you can defend) and re-post the verdict" }], details: { ...details(), error: "approve-with-frontier" } };
			}
			if (!store) return { content: [{ type: "text", text: "no coverage store configured — the declaration lives in the ledger only" }], details: details() };
			try {
				const prNum = readPrContext(dir).number ?? 0;
				const storePath = `${store}/coverage/${prNum}.json`;
				let prev: any = {};
				try {
					prev = JSON.parse(readFileSync(storePath, "utf8"));
					if (typeof prev !== "object" || prev === null) prev = {};
				} catch {
					/* first round */
				}
				const byPath: Record<string, any> = {};
				for (const e of (prev.covered ?? []) as any[]) {
					if (e && typeof e.path === "string") byPath[e.path] = e;
				}
				const round = Number(prev.rounds ?? 0) + 1;
				const sha = String(ledger.reviewed_sha ?? "");
				for (const e of params.covered) {
					const entry: any = { path: e.path, sha, round };
					if (e.note && e.note.trim()) entry.note = e.note;
					byPath[e.path] = entry;
				}
				// Frontier per-path (#716 r2/r3): a carried frontier path
				// leaves by being EXAMINED (declared covered this round) or
				// by LEAVING THE CHANGED SURFACE — the honest exit for a
				// same-head re-check round. Empty/absent files_changed is
				// UNKNOWABLE, not "nothing changed": nothing retires.
				const declaredCovered = new Set(params.covered.map((e) => e.path));
				let changedSurface: Set<string> | null = null;
				try {
					const ctx = JSON.parse(readFileSync(`${dir}/pr-context.json`, "utf8"));
					const fc = (ctx.files_changed ?? []) as any[];
					if (Array.isArray(fc) && fc.length > 0) {
						changedSurface = new Set(fc.filter((f) => f && typeof f.filename === "string").map((f) => f.filename));
					}
				} catch {
					changedSurface = null;
				}
				const frontierOut: Record<string, any> = {};
				for (const e of params.frontier) {
					frontierOut[e.path] = { path: e.path, reason: e.reason ?? "not examined" };
				}
				let retired = 0;
				for (const pe of (prev.frontier ?? []) as any[]) {
					if (!pe || typeof pe.path !== "string") continue;
					if (declaredCovered.has(pe.path) || frontierOut[pe.path]) continue;
					if (changedSurface !== null && !changedSurface.has(pe.path)) {
						retired++;
						continue;
					}
					frontierOut[pe.path] = { path: pe.path, reason: pe.reason ?? "not examined" };
				}
				const merged = {
					version: 1,
					pr: prNum,
					rounds: round,
					last_reviewed_sha: sha,
					updated_at: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
					covered: Object.values(byPath).sort((a: any, b: any) => (a.path < b.path ? -1 : 1)),
					frontier: Object.values(frontierOut).sort((a: any, b: any) => (a.path < b.path ? -1 : 1)),
				};
				const retiredNote = retired ? ` (${retired} frontier path(s) retired — left the changed surface)` : "";
				mkdirSync(dirname(storePath), { recursive: true });
				const tmp = `${storePath}.tmp`;
				writeFileSync(tmp, JSON.stringify(merged, null, 2) + "\n");
				renameSync(tmp, storePath);
				return { content: [{ type: "text", text: `coverage merged: round ${round}, covered ${params.covered.length}, frontier ${params.frontier.length}${retiredNote} — ${storePath}` }], details: details() };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `coverage merge failed (non-fatal — the verdict stands): ${msg}` }], details: { ...details(), exportError: msg } };
			}
		},
	});

	// ── carry_file — architecture-scale items escape the PR as ONE deduped ──
	pi.registerTool({
		name: "review_carry_file",
		label: "File carries",
		description:
			"Close out the round's architecture-carry items: for each, mint the deduped tracker issue ONLY when the finding is severe/structural (or marked mint:) — otherwise carry in the review record — then reply on the thread and RESOLVE it. Call once at round end, after the verdict. A failed resolve leaves the thread open: the gate counts it (fail-closed).",
		parameters: Type.Object({
			extra_threads: Type.Optional(Type.Array(Type.Object({ thread_ref: Type.String(), body: Type.String() }))),
		}),
		execute: async (_id, params) => {
			const t = target(process.env);
			if ("err" in t) return { content: [{ type: "text", text: t.err }], details: { ...details(), error: t.err } };
			const ARCH_MS = "Architecture & Design improvements";
			const STOP = new Set("the a an and or but not is are was were be been being this that these those it its with without for from into over under again further then once here there when where why how all any both each few more most other some such only own same so than too very can will just dont should now still has have had also while".split(" "));
			const CLASS_PREFIX = /^\s*(?:\**(?:architecture|architecture-carry|todo|blocking)\**\s*[:\u2014-]?\s*)+/i;
			const ROUND_NOISE = /\([^)]*(?:round|unchanged|narrowed|unaddressed|still open|already minted|do not create)[^)]*\)/gi;
			const RECURRENCE = /\(\s*\d+(?:st|nd|rd|th)?\s+round|\bround\s+\d+\b|\b(?:unchanged|narrowed|already minted|do not create|recurring)\b/i;
			const MINT_WORTHY = /\b(?:severe|security|data[- ]?loss|outage|corruption)\b|^mint:|\smint:/im;
			const GENERATED = /raw\/arch\/|model\.c4|\.mmd|rig\.json|arch[- ]sync|arch graph|update architecture page/i;
			const normTokens = (title: string): string[] =>
				title
					.replace(CLASS_PREFIX, "")
					.replace(ROUND_NOISE, " ")
					.toLowerCase()
					.replace(/[^a-z0-9]+/g, " ")
					.split(" ")
					.filter((w) => w.length > 2 && !STOP.has(w));
			try {
				const known = await t.c.listIssues();
				const milestones = await t.c.listMilestones();
				const ms = milestones.find((m) => m.title.trim() === ARCH_MS);
				const findDup = (title: string): number | null => {
					const a = normTokens(title);
					if (!a.length) return null;
					const sa = new Set(a);
					const ga = new Set(a.slice(1).map((_, i) => a[i] + " " + a[i + 1]));
					for (const k of known) {
						const b = normTokens(k.title);
						if (!b.length) continue;
						const sb = new Set(b);
						const gb = new Set(b.slice(1).map((_, i) => b[i] + " " + b[i + 1]));
						const inter = [...sa].filter((x) => sb.has(x)).length;
						if (inter > 0 && inter / new Set([...sa, ...sb]).size >= 0.55) return k.number;
						const gi = [...ga].filter((x) => gb.has(x)).length;
						if (ga.size > 0 && gb.size > 0 && gi / new Set([...ga, ...gb]).size >= 0.45) return k.number;
						if (a.length >= 4 && [...sa].every((x) => sb.has(x))) return k.number;
						if (b.length >= 4 && [...sb].every((x) => sa.has(x))) return k.number;
					}
					return null;
				};
				const bestOverlap = (title: string): number | null => {
					const a = new Set(normTokens(title));
					let best: number | null = null;
					let hits = 1;
					for (const k of known) {
						const n = [...a].filter((x) => normTokens(k.title).includes(x)).length;
						if (n > hits) {
							hits = n;
							best = k.number;
						}
					}
					return best;
				};
				const ensureIssue = (title: string, body: string): { num: number | null; outcome: string } => {
					const dup = findDup(title);
					if (dup !== null) return { num: dup, outcome: "referenced" };
					if (GENERATED.test(title) || GENERATED.test(body)) return { num: null, outcome: "generated" };
					if (RECURRENCE.test(title) || RECURRENCE.test(body)) {
						const cand = bestOverlap(title);
						return cand !== null ? { num: cand, outcome: "referenced" } : { num: null, outcome: "recurrence" };
					}
					if (!MINT_WORTHY.test(body)) return { num: null, outcome: "unmarked" };
					return { num: null, outcome: "mint-pending" };
				};
				const notes: string[] = [];
				const minted: { issue: number; title: string }[] = [];
				const resolved: string[] = [];
				const failed: string[] = [];
				let ledgerTodos: any[] = [];
				try {
					ledgerTodos = JSON.parse(readFileSync(`${process.env.HARMOSTES_WORKDIR ?? "/workspace"}/review.json`, "utf8")).todos ?? [];
				} catch {
					/* no ledger — threads only */
				}
				const entries: { title: string; body: string; thread_ref?: string; preRef?: string }[] = [];
				for (const td of ledgerTodos) {
					if (td && td.disposition === "architecture-carry" && td.body) {
						entries.push({ title: "architecture: " + String(td.body).split("\n")[0].slice(0, 140), body: String(td.body), thread_ref: td.thread_ref, preRef: td.issue });
					}
				}
				for (const f of posted.filter((x) => x.severity === "Architecture-carry")) {
					entries.push({ title: "architecture: " + f.body.split("\n")[0].slice(0, 140), body: f.body, thread_ref: f.thread_ref });
				}
				for (const x of params.extra_threads ?? []) {
					entries.push({ title: "architecture: " + x.body.replace(/\*\*Architecture-carry\*\*\s*/, "").split("\n")[0].slice(0, 140), body: x.body, thread_ref: x.thread_ref });
				}
				for (const e of entries) {
					let num: number | null = null;
					let note: string | undefined;
					const refDigits = e.preRef?.match(/\d+/)?.[0];
					if (refDigits) {
						num = Number(refDigits);
					} else {
						const outcome = ensureIssue(e.title, `from PR #${t.t.number} — ${e.body}\n(carried by the review pipeline)`);
						if (outcome.outcome === "mint-pending") {
							try {
								num = await t.c.createIssue(e.title, `from PR #${t.t.number} — ${e.body}\n(carried by the review pipeline)`, ms?.id);
								minted.push({ issue: num, title: e.title });
								known.push({ number: num, title: e.title }); // in-run dedup
							} catch (err) {
								notes.push(`carry mint FAILED for: ${e.title.slice(0, 60)}`);
								failed.push(e.thread_ref ?? e.title);
								continue;
							}
						} else if (outcome.outcome === "referenced") {
							num = outcome.num;
							notes.push(`carry references existing issue #${num} — no re-mint: ${e.title.slice(0, 60)}`);
						} else if (outcome.outcome === "generated") {
							note = "carried in the review record — generated artifact, owned by its generator (arch-sync), not the tracker";
							notes.push(`carry SKIPPED mint (generated): ${e.title.slice(0, 60)}`);
						} else if (outcome.outcome === "recurrence") {
							note = "carried in the review record — declared recurrence with no covering issue found; raise one manually only if this is genuinely new";
							notes.push(`carry SKIPPED mint (recurrence): ${e.title.slice(0, 60)}`);
						} else {
							note = "carried in the review record — no tracker issue minted (minting is the exception; mark severe/structural or add 'mint:' to promote)";
							notes.push(`carry SKIPPED mint (unmarked): ${e.title.slice(0, 60)}`);
						}
					}
					if (e.thread_ref) {
						const replyText = note ?? `carried to #${num} — tracked as architecture follow-up; the thread closes here`;
						try {
							await t.c.reply(e.thread_ref, replyText);
						} catch (err) {
							notes.push(`reply failed on ${e.thread_ref}: ${err instanceof Error ? err.message : String(err)}`);
						}
						try {
							await t.c.resolve(e.thread_ref);
							resolved.push(e.thread_ref);
						} catch {
							notes.push(`resolve FAILED for thread ${e.thread_ref} (${num ? "issue #" + num : "review-record carry"}) — the thread stays OPEN and the gate counts it`);
							failed.push(e.thread_ref);
						}
					}
				}
				const summary = `carry: ${entries.length} entr(ies), ${minted.length} minted, ${resolved.length} resolved, ${failed.length} failed` + (notes.length ? `\n${notes.join("\n")}` : "");
				return { content: [{ type: "text", text: summary }], details: { ...details(), carry: { minted, resolved, failed, notes } } };
			} catch (e) {
				const msg = e instanceof Error ? e.message : String(e);
				return { content: [{ type: "text", text: `carry failed: ${msg} — entries stay open; the gate counts them` }], details: { ...details(), error: msg } };
			}
		},
	});
}
