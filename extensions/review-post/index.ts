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
import { readFileSync, writeFileSync, renameSync } from "node:fs";
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
}
