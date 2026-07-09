#!/usr/bin/env python3
"""
DEPRECATED — kept only until the llm-wiki + fork-maintenance pipelines port to the
Go harmostes framework (internal/agent), at which point this file is deleted.

The production primitive is now the Go port (github.com/tibrezus/harmostes,
internal/agent) — tested to be behaviorally equivalent (task → gate → warm-session
feedback). This Python file was an expedient first cut (pi is a Node tool with a
Node/TypeScript SDK; Python was never the right language for a framework
primitive). It remains in the runtime path of the LIVE llm-wiki controller
(agent-sync.sh) and fork-maintenance resolver (resolve-conflict.sh) until those
become harmostes Workflows.

Original description follows:

harmostes — shared pi.dev RPC orchestration for automated agent workflows.

The core primitive, shared by llm-wiki and fork-maintenance:

    run an agent task → run a GATE → on failure, feed the gate's error back to
    the SAME pi session (the agent keeps its context) → re-run the gate, up to
    N fixes. Only a green gate "counts"; otherwise escalate.

This replaces the cold `pi --print` re-invocations both pipelines used: instead
of restarting the agent from scratch on each validation failure, the fix is a
continuation of one warm RPC session (the agent remembers what it just did), and
the orchestrator gets full event observability (every tool call) instead of only
final text.

It speaks pi's RPC JSONL protocol (https://pi.dev/docs/latest/rpc) over a
`pi --mode rpc` subprocess — reusing pi's own --skill/--model/--tools handling
(provider, auth, skill loading, tool allowlist) rather than re-implementing them.
The pi SDK (createAgentSession) is the in-process Node equivalent; RPC is the
language-agnostic choice for a CLI callable from bash + Python workflows.

Usage:
  harmostes task \
    --skill /path/to/SKILL.md --model zai/glm-5.2 --tools read,bash,edit \
    --workdir /repo --task-file task.txt \
    --gate "bash validate.sh && grep -q SIG file" \
    [--max-fixes 3] [--log /path/events.log] [--no-session]

Env: the model's API key (e.g. ZAI_API_KEY) must be set; it is passed through to pi.
Exit: 0 = gate green; 1 = gate failed after --max-fixes; 2 = agent/pi error.
"""
import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone


def log(msg: str) -> None:
    print(f"[harmostes] {datetime.now(timezone.utc).isoformat()} {msg}", flush=True)


class PiRpc:
    """A thin client over `pi --mode rpc`: send JSONL commands, read JSONL events."""

    def __init__(self, args, cwd, env, log_path):
        self.log_path = log_path
        self._logf = open(log_path, "a") if log_path else None
        self.proc = subprocess.Popen(
            ["pi", "--mode", "rpc", "--no-session", *args],
            cwd=cwd, env=env,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            bufsize=0,
        )
        self._buf = b""
        self.tool_calls = 0

    def _send(self, obj: dict) -> None:
        line = (json.dumps(obj) + "\n").encode()
        self.proc.stdin.write(line)
        self.proc.stdin.flush()

    def _read_event(self):
        """Read one JSONL event from stdout (split on \\n only — protocol-compliant)."""
        while b"\n" not in self._buf:
            chunk = self.proc.stdout.read(4096)
            if not chunk:
                if self._buf:
                    line, self._buf = self._buf, b""
                    return self._parse(line)
                return None
            self._buf += chunk
        line, self._buf = self._buf.split(b"\n", 1)
        return self._parse(line)

    @staticmethod
    def _parse(line: bytes):
        line = line.strip()
        if not line:
            return None
        try:
            return json.loads(line.decode())
        except Exception:
            return {"type": "_unparseable", "raw": line.decode(errors="replace")}

    def _emit(self, event: dict) -> None:
        if self._logf:
            self._logf.write(json.dumps(event) + "\n")
            self._logf.flush()

    def prompt(self, message: str, label: str = "task") -> dict:
        """Send a prompt and block until the agent finishes the resulting work
        (agent_end). Returns the agent_end event (or the last event on error)."""
        log(f"→ prompt ({label})")
        self._send({"type": "prompt", "message": message})
        last = None
        while True:
            ev = self._read_event()
            if ev is None:
                log("  (pi stdout closed)")
                return last or {"type": "_closed"}
            self._emit(ev)
            t = ev.get("type")
            if t == "tool_execution_start":
                self.tool_calls += 1
                log(f"  tool: {ev.get('toolName')} {str(ev.get('args',''))[:80]}")
            elif t == "agent_end":
                log(f"  agent_end ({self.tool_calls} tool calls total this turn-set)")
                return ev
            elif t == "response" and not ev.get("success", True):
                log(f"  command rejected: {ev}")
            elif t == "_unparseable":
                log(f"  (unparseable line: {ev.get('raw','')[:120]})")
            last = ev

    def dispose(self) -> None:
        try:
            self._send({"type": "abort"})
        except Exception:
            pass
        try:
            self.proc.stdin.close()
        except Exception:
            pass
        try:
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()
        if self._logf:
            self._logf.close()


def run_gate(command: str, cwd: str) -> tuple[int, str]:
    """Run the gate command; return (exit_code, combined_output)."""
    log(f"gate: {command}")
    r = subprocess.run(command, shell=True, cwd=cwd, capture_output=True, text=True)
    out = (r.stdout + r.stderr).strip()
    return r.returncode, out


def main():
    ap = argparse.ArgumentParser(prog="harmostes")
    sub = ap.add_subparsers(dest="cmd", required=True)

    t = sub.add_parser("task", help="agent task + gate + feedback-as-session-continuation")
    t.add_argument("--skill", required=True, help="path to SKILL.md")
    t.add_argument("--model", default="zai/glm-5.2")
    t.add_argument("--tools", default="read,bash,edit,grep",
                   help="comma-separated tool allowlist (pi has no per-tool approval/sandbox)")
    t.add_argument("--workdir", required=True, help="agent working directory (the repo)")
    t.add_argument("--task-file", required=True, help="file with the initial task prompt")
    t.add_argument("--gate", required=True, help="shell command run after each agent turn; exit 0 = pass")
    t.add_argument("--max-fixes", type=int, default=3)
    t.add_argument("--log", default=None, help="append full event stream here")
    t.add_argument("--timeout", type=int, default=1800, help="per-turn pi timeout (seconds)")
    args = ap.parse_args()

    if args.cmd != "task":
        ap.error("unknown command")

    with open(args.task_file) as f:
        task = f.read().strip()
    if not task:
        log("ERROR: empty task"); sys.exit(2)

    env = os.environ.copy()
    workdir = os.path.abspath(args.workdir)
    os.makedirs(workdir, exist_ok=True)

    pi_args = [
        "--skill", args.skill,
        "--model", args.model,
        "--tools", args.tools,
    ]
    log(f"starting pi --mode rpc (model={args.model}, tools={args.tools}, workdir={workdir})")

    rpc = PiRpc(pi_args, cwd=workdir, env=env, log_path=args.log)
    try:
        # turn 1: the task itself
        rpc.prompt(task, label="initial task")

        # feedback loop: gate → on failure, feed the error back to the SAME session
        for attempt in range(1, args.max_fixes + 1):
            rc, out = run_gate(args.gate, cwd=workdir)
            if rc == 0:
                log(f"✅ gate GREEN (after {attempt} pass(es))")
                sys.exit(0)
            log(f"❌ gate failed (exit {rc}) — pass {attempt}/{args.max_fixes}")
            if attempt >= args.max_fixes:
                break
            tail = "\n".join(out.splitlines()[-25:])
            feedback = (
                f"The validation gate just failed (exit {rc}). Output:\n\n{tail}\n\n"
                f"Fix it (you're still in {workdir} on the same branch). Adapt any "
                f"customization to upstream's current shape — do NOT drop it. Then stop; "
                f"do not merge or open PRs. The gate will re-run."
            )
            rpc.prompt(feedback, label=f"feedback #{attempt}")

        # final gate check after the last fix
        rc, out = run_gate(args.gate, cwd=workdir)
        if rc == 0:
            log(f"✅ gate GREEN (after {args.max_fixes} fix(es))")
            sys.exit(0)
        log(f"❌ gate still failing after {args.max_fixes} fixes — escalate")
        sys.exit(1)
    except Exception as e:
        log(f"ERROR: {e}")
        sys.exit(2)
    finally:
        rpc.dispose()


if __name__ == "__main__":
    main()
