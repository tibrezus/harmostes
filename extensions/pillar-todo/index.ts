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

import { StringEnum } from "@earendil-works/pi-ai";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { readFileSync } from "node:fs";
import { Type } from "typebox";

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
	action: StringEnum(["list", "add", "toggle", "clear"] as const),
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
	const reconstructState = (ctx: ExtensionContext) => {
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

	pi.on("session_start", async (_event, ctx) => {
		const sawTodoEntry = reconstructState(ctx);
		if (sawTodoEntry || todos.length > 0) return; // resumed: the branch IS the state
		// Fresh session: seed from the manifest when present.
		try {
			const dir = process.env.HARMOSTES_WORKDIR ?? "/workspace";
			const raw = readFileSync(`${dir}/pillar-todos.json`, "utf8");
			const items = parseSeedManifest(raw);
			if (items) {
				todos = items.map((text) => ({ id: nextId++, text, done: false }));
			}
		} catch {
			// no manifest / unreadable: empty list, tool still works
		}
	});

	pi.registerTool({
		name: "todo",
		label: "Todo",
		description: "Manage a todo list. Actions: list, add (text), toggle (id), clear",
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
			}
		},
	});
}
