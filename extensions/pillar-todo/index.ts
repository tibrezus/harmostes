/**
 * pillar-todo — pi extension: the reviewer's pillar checklist, seeded
 * before the session starts (#727 follow-up).
 *
 * The todo tool is pi's own example contract (examples/extensions/todo.ts,
 * MIT): actions list/add/toggle/clear, state carried in tool-result
 * details so branching reconstructs the right list. This variant adds one
 * thing the fleet needs: on session_start, when the transcript carries NO
 * todo entries (a fresh session — never a resumed one, whose state
 * reconstructs from the branch), it seeds the list from
 * $HARMOSTES_WORKDIR/pillar-todos.json — the pillar checklist the
 * workspace plugin extracts from the SERVED pr-review playbook (the
 * skill's `## ` headings before "## Hardened checks"; the skill is the
 * single source of the pillar set, the plugin only parses it).
 *
 * Why seed instead of letting the agent build the list: the #728-era
 * rounds spent their first turns re-deriving the same checklist every
 * round. The walk order and what counts as "done" stay the skill's
 * methodology (playbook, Pillar scan); this extension only guarantees
 * the checklist EXISTS at turn one, with ticks persisting across rounds
 * through the session lineage.
 *
 * Seeding is best-effort and silent on absence: no manifest, an
 * unparsable one, or an empty one → empty list, the tool still works.
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { readFileSync } from "node:fs";
import { Type } from "typebox"; // pi aliases typebox for extensions — the rig-query form: a bare-specifier value import from @earendil-works/pi-ai resolves only through pi's loader alias and drags the whole pi closure onto the plain-node test path (review r1)

export interface Todo {
	id: number;
	text: string;
	done: boolean;
}

interface TodoDetails {
	action: "list" | "add" | "toggle" | "clear";
	todos: Todo[];
	nextId: number;
	error?: string;
}

const TodoParams = Type.Object({
	// Type.Unsafe enum (the rig-query form): the shape StringEnum builds,
	// without the pi-ai value import.
	action: Type.Unsafe<{ type: "string"; enum: string[] }>({
		type: "string",
		enum: ["list", "add", "toggle", "clear", "seed"],
	}),
	text: Type.Optional(Type.String({ description: "Todo text (for add)" })),
	id: Type.Optional(Type.Number({ description: "Todo ID (for toggle)" })),
});

/**
 * parseSeedManifest: the only shape /workspace/pillar-todos.json may
 * take — a JSON array of strings (or {text} objects, the plugin emits
 * strings). Anything else (object, number, unparsable) is NO manifest:
 * null, never a throw — a malformed seed must not take down the tool.
 */
export function parseSeedManifest(raw: string): string[] | null {
	try {
		const parsed = JSON.parse(raw);
		if (!Array.isArray(parsed)) return null;
		const items: string[] = [];
		for (const entry of parsed) {
			if (typeof entry === "string" && entry.trim() !== "") items.push(entry.trim());
			else if (entry && typeof entry === "object" && typeof (entry as { text?: unknown }).text === "string" && (entry as { text: string }).text.trim() !== "")
				items.push(((entry as { text: string }).text).trim());
			else return null; // one bad entry poisons the manifest — not a checklist
		}
		return items.length > 0 ? items : null;
	} catch {
		return null;
	}
}

export default function (pi: ExtensionAPI) {
	let todos: Todo[] = [];
	let nextId = 1;

	/** Reconstruct from the session branch (the example's contract). */
	const reconstructState = (ctx: ExtensionContext): void => {
		todos = [];
		nextId = 1;
		let sawTodoEntry = false;
		for (const entry of ctx.sessionManager.getBranch()) {
			if (entry.type !== "message") continue;
			const msg = entry.message;
			if (msg.role !== "toolResult" || msg.toolName !== "todo") continue;
			sawTodoEntry = true;
			const details = msg.details as TodoDetails | undefined;
			if (details) {
				todos = details.todos;
				nextId = details.nextId;
			}
		}
		return sawTodoEntry;
	};

	/**
	 * Seed from the workspace manifest; false when there is nothing to
	 * seed (absent/unparsable/empty manifest).
	 */
	const seedFromManifest = (): boolean => {
		try {
			const dir = process.env.HARMOSTES_WORKDIR ?? "/workspace";
			const raw = readFileSync(`${dir}/pillar-todos.json`, "utf8");
			const items = parseSeedManifest(raw);
			if (items) {
				todos = items.map((text) => ({ id: nextId++, text, done: false }));
				return true;
			}
		} catch {
			// no manifest / unreadable: empty list, tool still works
		}
		return false;
	};

	pi.on("session_start", async (_event, ctx) => {
		reconstructState(ctx);
		// Suppress the seed only when the branch CARRIES todo history —
		// any todo toolResult (even a `clear`, whose details.todos is [])
		// means the lineage owns the state. A branch with none (fresh
		// session, or a round that died before ever calling the tool) is
		// indistinguishable from never-seeded and gets a fresh checklist
		// (review r1: the todos.length disjunct was dead — reconstruct
		// only fills todos when it also saw an entry).
		const branchHasTodoHistory = ctx.sessionManager
			.getBranch()
			.some(
				(e) =>
					e.type === "message" &&
					(e.message as { role?: string; toolName?: string } | undefined)?.role === "toolResult" &&
					(e.message as { toolName?: string }).toolName === "todo",
				);
		if (branchHasTodoHistory) return;
		seedFromManifest();
	});

	pi.registerTool({
		name: "todo",
		label: "Todo",
		description: "Manage a todo list. Actions: list, add (text), toggle (id), clear, seed (re-read the pillar manifest)",
		parameters: TodoParams,

		async execute(_toolCallId, params) {
			switch (params.action) {
				case "list":
					return {
						content: [
							{
								type: "text",
								text: todos.length
									? todos.map((t) => `[${t.done ? "x" : " "}] #${t.id}: ${t.text}`).join("\n")
									: "No todos",
							},
						],
						details: { action: "list", todos: [...todos], nextId } as TodoDetails,
					};
				case "add": {
					if (!params.text) {
						return {
							content: [{ type: "text", text: "Error: text required for add" }],
							details: { action: "add", todos: [...todos], nextId, error: "text required" } as TodoDetails,
						};
					}
					const newTodo: Todo = { id: nextId++, text: params.text, done: false };
					todos.push(newTodo);
					return {
						content: [{ type: "text", text: `Added todo #${newTodo.id}: ${newTodo.text}` }],
						details: { action: "add", todos: [...todos], nextId } as TodoDetails,
					};
				}
				case "toggle": {
					if (params.id === undefined) {
						return {
							content: [{ type: "text", text: "Error: id required for toggle" }],
							details: { action: "toggle", todos: [...todos], nextId, error: "id required" } as TodoDetails,
						};
					}
					const todo = todos.find((t) => t.id === params.id);
					if (!todo) {
						return {
							content: [{ type: "text", text: `Error: #${params.id} not found` }],
							details: { action: "toggle", todos: [...todos], nextId, error: `#${params.id} not found` } as TodoDetails,
						};
					}
					todo.done = !todo.done;
					return {
						content: [{ type: "text", text: `Todo #${todo.id} ${todo.done ? "completed" : "uncompleted"}` }],
						details: { action: "toggle", todos: [...todos], nextId } as TodoDetails,
					};
				}
				case "clear": {
					const count = todos.length;
					todos = [];
					nextId = 1;
					return {
						content: [{ type: "text", text: `Cleared ${count} todos` }],
						details: { action: "clear", todos: [], nextId: 1 } as TodoDetails,
					};
				}
				case "seed": {
					// Re-read the manifest: the playbook's "clear and reseed
					// for a deliberate full re-sweep" must be executable
					// (review r1: session_start fires once per session —
					// without this a cleared list was a one-way door).
					// Clear FIRST, unconditionally — the action's contract is
					// replace-what-is-there, even when no manifest exists.
					const count = todos.length;
					todos = [];
					nextId = 1;
					const seeded = seedFromManifest();
					return {
						content: [
							{
								type: "text",
								text: seeded
									? `Replaced ${count} todo(s) with ${todos.length} from the pillar manifest`
									: `Cleared ${count} todo(s); no pillar manifest to reseed from`,
							},
						],
						details: { action: "clear", todos: [...todos], nextId } as TodoDetails,
					};
				}
			}
		},
	});
}
