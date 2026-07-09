// Command harmostes-agent is the Go port of the proven harmostes.py primitive:
// run an agent task → run a GATE → on failure feed the gate's stderr back to the
// SAME warm pi session → re-run the gate, up to N fixes.
//
// It is the standalone form of the framework's agent step (the worker runs the
// same loop from a Workflow CR). Usage mirrors harmostes.py so the two are
// behaviorally interchangeable:
//
//	harmostes-agent task \
//	  --skill /skills/wiki/SKILL.md --model zai/glm-5.2 --tools read,bash,edit,grep \
//	  --workdir /repo --task-file task.txt \
//	  --gate "bash gate.sh /repo" \
//	  [--max-fixes 3] [--log events.jsonl] [--timeout 1800]
//
// Exit: 0 gate green · 1 gate failed after --max-fixes · 2 agent/pi error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tibrezus/harmostes/internal/agent"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "task" {
		fmt.Fprintln(os.Stderr, "usage: harmostes-agent task --skill SK --model M --tools T --workdir DIR --task-file F --gate CMD")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("task", flag.ExitOnError)
	skill := fs.String("skill", "", "path to SKILL.md")
	model := fs.String("model", "zai/glm-5.2", "model id")
	tools := fs.String("tools", "read,bash,edit,grep", "comma-separated tool allowlist")
	workdir := fs.String("workdir", "", "agent working directory (the repo)")
	taskFile := fs.String("task-file", "", "file with the initial task prompt")
	gate := fs.String("gate", "", "shell command run after each agent turn; exit 0 = green")
	maxFixes := fs.Int("max-fixes", 3, "max feedback attempts")
	logPath := fs.String("log", "", "append the full event stream (JSONL) here")
	timeout := fs.Int("timeout", 1800, "per-run timeout (seconds)")
	_ = fs.Parse(os.Args[2:])

	*workdir = abs(*workdir)
	if *skill == "" || *workdir == "" || *taskFile == "" || *gate == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --skill, --workdir, --task-file, --gate are required")
		os.Exit(2)
	}
	taskBytes, err := os.ReadFile(*taskFile)
	if err != nil {
		hlog("ERROR: read task-file: %v", err)
		os.Exit(2)
	}
	task := string(taskBytes)
	if task == "" {
		hlog("ERROR: empty task")
		os.Exit(2)
	}
	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		hlog("ERROR: mkdir workdir: %v", err)
		os.Exit(2)
	}

	// event logger: stderr (human) + optional JSONL file
	var logFile *os.File
	if *logPath != "" {
		logFile, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			hlog("ERROR: open log: %v", err)
			os.Exit(2)
		}
		defer logFile.Close()
	}
	logger := func(ev agent.Event) {
		hlog("  %s %s", ev.Type, toolSuffix(ev))
		if logFile != nil {
			b, _ := json.Marshal(ev)
			logFile.Write(append(b, '\n'))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()

	hlog("starting pi --mode rpc (model=%s tools=%s workdir=%s)", *model, *tools, *workdir)
	rpc, err := agent.NewRPC(ctx, agent.RPCOptions{
		Args:    []string{"--skill", *skill, "--model", *model, "--tools", *tools},
		Workdir: *workdir,
		Env:     os.Environ(),
		Log:     logger,
	})
	if err != nil {
		hlog("ERROR: start pi: %v", err)
		os.Exit(2)
	}
	defer rpc.Abort(context.Background())

	g := agent.CmdGate{Command: *gate, Dir: *workdir}
	res, err := agent.Task(ctx, rpc, g, task, *maxFixes, logger)
	if err != nil {
		hlog("ERROR: %v", err)
		os.Exit(2)
	}
	if res.Green {
		hlog("✅ gate GREEN (after %d pass(es))", res.Attempts)
		os.Exit(0)
	}
	hlog("❌ gate still failing after %d evaluation(s) — escalate", res.Attempts)
	os.Exit(1)
}

func hlog(format string, args ...any) {
	log.Printf("[harmostes] "+format, args...)
}

func toolSuffix(ev agent.Event) string {
	if ev.ToolName != "" {
		return ev.ToolName
	}
	return ""
}

func abs(p string) string {
	if p == "" {
		return p
	}
	out, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return out
}
