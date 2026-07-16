package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/tibrezus/harmostes/internal/observability"
)

// withTestTracer installs an in-memory span exporter (synchronous) as the global
// tracer for the test. Restored on cleanup.
func withTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

func spanNameSet(spans []tracetest.SpanStub) map[string]bool {
	m := make(map[string]bool, len(spans))
	for _, s := range spans {
		m[s.Name] = true
	}
	return m
}

// spanContains reports whether secret appears in any span name or attribute value
// (used by the privacy test — decision #4: no raw bodies leak into telemetry).
func spanContains(spans []tracetest.SpanStub, secret string) bool {
	for _, s := range spans {
		if strings.Contains(s.Name, secret) {
			return true
		}
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.Emit(), secret) {
				return true
			}
		}
	}
	return false
}

// TestTaskEmitsTurnAndGateSpans: a fail-then-pass run emits agent.task +
// agent.feedback#1 turn spans and gate.evaluate spans; the passing gate carries
// green=true. (No tool spans here — fakeSession doesn't emit tool events; those
// are covered by the RPC test below.)
func TestTaskEmitsTurnAndGateSpans(t *testing.T) {
	exp := withTestTracer(t)
	ctx := observability.WithWorkflow(context.Background(), "wf-test")
	sess := &fakeSession{}
	gate := &scriptedGate{greens: []bool{false, true}, outputs: []string{"error: bad", ""}}

	res, err := Task(ctx, sess, gate, "do thing", 3, nil)
	if err != nil || !res.Green {
		t.Fatalf("expected green: %+v err=%v", res, err)
	}

	spans := exp.GetSpans()
	names := spanNameSet(spans)
	for _, want := range []string{"agent.task", "agent.feedback#1", "gate.evaluate"} {
		if !names[want] {
			t.Errorf("missing span %q (have %v)", want, names)
		}
	}
	// a failing gate.evaluate carries green=false + feedback_chars>0
	var sawFail, sawPass bool
	for _, s := range spans {
		if s.Name != "gate.evaluate" {
			continue
		}
		green, hasGreen := attrBool(s, "harmostes.green")
		if !hasGreen {
			continue
		}
		if green {
			sawPass = true
		} else {
			sawFail = true
		}
	}
	if !sawFail || !sawPass {
		t.Errorf("expected both a failing and a passing gate.evaluate span (fail=%v pass=%v)", sawFail, sawPass)
	}
}

func attrBool(s tracetest.SpanStub, key string) (bool, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsBool(), true
		}
	}
	return false, false
}

// fakePiSecretArg writes a fake pi that emits ONE tool call whose args embed a
// secret, then agent_end, per stdin prompt. Lets the privacy test assert the
// secret never reaches a span (only args_chars).
func fakePiSecretArg(t *testing.T, secretArg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakepi-secret")
	// args.file = <secretArg>; the secret is an opaque token (no JSON metachars).
	script := strings.Join([]string{
		"#!/usr/bin/env bash",
		`while IFS= read -r line; do`,
		`  case "$line" in *'"type":"abort"'*) exit 0 ;; esac`,
		`  echo '{"type":"tool_execution_start","toolName":"read","args":{"file":"'` + secretArg + `'"}}'`,
		`  echo '{"type":"tool_execution_end","toolName":"read","success":true}'`,
		`  echo '{"type":"agent_end"}'`,
		`done`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTelemetryNeverLeaksBodies (decision #4): a run whose task message, gate
// feedback, and tool args each contain a distinct secret MUST NOT emit any of
// those secrets into span names or attributes — only sizes (message_chars,
// feedback_chars, args_chars) and names/labels. This is the hard privacy guard.
func TestTelemetryNeverLeaksBodies(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	exp := withTestTracer(t)

	const (
		secretTask = "SECRET_TASK_BODY_77"
		secretArg  = "SECRET_ARGS_BODY_88"
		secretGate = "SECRET_GATE_FEEDBACK_99"
	)
	ctx := observability.WithWorkflow(context.Background(), "wf-priv")
	rpc, err := NewRPC(ctx, RPCOptions{PiPath: fakePiSecretArg(t, secretArg), Workdir: "."})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Abort(context.Background())

	// maxFixes=1: turn 1 (task) → gate fails (secret feedback) → break → final gate passes.
	// So all three secret surfaces are exercised: task message, gate feedback, tool args.
	gate := &scriptedGate{greens: []bool{false, true}, outputs: []string{secretGate, ""}}

	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Task(observability.WithWorkflow(pctx, "wf-priv"), rpc, gate, "do thing "+secretTask, 1, nil); err != nil {
		t.Fatalf("Task: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected spans to be emitted (telemetry active)")
	}
	// a tool span WAS emitted — so the absence of the arg-secret is meaningful.
	if !spanNameSet(spans)["read"] {
		t.Errorf("expected a 'read' tool span; got %v", spanNameSet(spans))
	}
	for _, secret := range []string{secretTask, secretArg, secretGate} {
		if spanContains(spans, secret) {
			t.Errorf("decision #4 violation: secret %q leaked into telemetry:\n%s",
				secret, dumpSpans(spans))
		}
	}
	// positive: the size-only attrs ARE present (proving we measured, not just dropped).
	if !anySpanHasAttr(spans, "harmostes.args_chars") {
		t.Errorf("expected harmostes.args_chars on the tool span")
	}
	if !anySpanHasAttr(spans, "harmostes.feedback_chars") {
		t.Errorf("expected harmostes.feedback_chars on a gate span")
	}
}

func dumpSpans(spans []tracetest.SpanStub) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Name + " {")
		for _, a := range s.Attributes {
			b.WriteString(string(a.Key) + "=" + a.Value.Emit() + " ")
		}
		b.WriteString("}\n")
	}
	return b.String()
}

func anySpanHasAttr(spans []tracetest.SpanStub, key string) bool {
	for _, s := range spans {
		for _, a := range s.Attributes {
			if string(a.Key) == key {
				return true
			}
		}
	}
	return false
}
