package graph

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
)

// ---------------------------------------------------------------------------
// Config types
// ---------------------------------------------------------------------------

// DaprStateGetConfig reads a key from a Dapr state store.
type DaprStateGetConfig struct {
	Store string `json:"store"` // Dapr state store component name (e.g. "harmostes-state")
	Key   string `json:"key"`   // state key (supports template expressions)
}

// DaprStateSetConfig writes a key to a Dapr state store.
type DaprStateSetConfig struct {
	Store string `json:"store"`
	Key   string `json:"key"`   // supports template expressions
	Value string `json:"value"` // supports template expressions
}

// DaprPublishConfig publishes a JSON message to a Dapr pub/sub topic.
type DaprPublishConfig struct {
	Pubsub  string `json:"pubsub"`  // Dapr pub/sub component name (e.g. "harmostes-pubsub")
	Topic   string `json:"topic"`   // topic name
	Payload string `json:"payload"` // JSON payload (supports template expressions)
}

// ---------------------------------------------------------------------------
// StateGetExecutor
// ---------------------------------------------------------------------------

// StateGetExecutor runs a "dapr-state-get" node — reads a key from a Dapr state
// store and outputs the value. A missing key is not an error; the output value
// is an empty string.
type StateGetExecutor struct {
	client dapr.Client
}

// NewStateGetExecutor creates a dapr-state-get node executor.
func NewStateGetExecutor(client dapr.Client) *StateGetExecutor {
	return &StateGetExecutor{client: client}
}

func (e *StateGetExecutor) Type() string        { return "dapr-state-get" }
func (e *StateGetExecutor) Deterministic() bool { return true }

func (e *StateGetExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.dapr-state-get")
	defer span.End()
	span.SetAttributes(attribute.String("harmostes.node.id", node.ID))

	if e.client == nil {
		return errNoDaprClient(span)
	}

	cfg, err := parseConfig[DaprStateGetConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	key := resolveTemplate(cfg.Key, env.Inputs)

	span.SetAttributes(
		attribute.String("harmostes.dapr.store", cfg.Store),
		attribute.String("harmostes.dapr.key", key),
	)

	value, err := e.client.GetState(ctx, cfg.Store, key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: "dapr get-state: " + err.Error()}, err
	}

	span.SetAttributes(attribute.Int("harmostes.dapr.value_len", len(value)))

	return NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"value": value},
	}, nil
}

// ---------------------------------------------------------------------------
// StateSetExecutor
// ---------------------------------------------------------------------------

// StateSetExecutor runs a "dapr-state-set" node — writes a key to a Dapr state
// store. Both key and value support template expressions resolved against
// upstream node outputs.
type StateSetExecutor struct {
	client dapr.Client
}

// NewStateSetExecutor creates a dapr-state-set node executor.
func NewStateSetExecutor(client dapr.Client) *StateSetExecutor {
	return &StateSetExecutor{client: client}
}

func (e *StateSetExecutor) Type() string        { return "dapr-state-set" }
func (e *StateSetExecutor) Deterministic() bool { return true }

func (e *StateSetExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.dapr-state-set")
	defer span.End()
	span.SetAttributes(attribute.String("harmostes.node.id", node.ID))

	if e.client == nil {
		return errNoDaprClient(span)
	}

	cfg, err := parseConfig[DaprStateSetConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	key := resolveTemplate(cfg.Key, env.Inputs)
	value := resolveTemplate(cfg.Value, env.Inputs)

	span.SetAttributes(
		attribute.String("harmostes.dapr.store", cfg.Store),
		attribute.String("harmostes.dapr.key", key),
		attribute.Int("harmostes.dapr.value_len", len(value)),
	)

	if err := e.client.SaveState(ctx, cfg.Store, key, value); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: "dapr save-state: " + err.Error()}, err
	}

	return NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"key": key},
	}, nil
}

// ---------------------------------------------------------------------------
// PublishExecutor
// ---------------------------------------------------------------------------

// PublishExecutor runs a "dapr-publish" node — publishes a JSON message to a
// Dapr pub/sub topic. The payload supports template expressions resolved
// against upstream node outputs.
type PublishExecutor struct {
	client dapr.Client
}

// NewPublishExecutor creates a dapr-publish node executor.
func NewPublishExecutor(client dapr.Client) *PublishExecutor {
	return &PublishExecutor{client: client}
}

func (e *PublishExecutor) Type() string        { return "dapr-publish" }
func (e *PublishExecutor) Deterministic() bool { return true }

func (e *PublishExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.dapr-publish")
	defer span.End()
	span.SetAttributes(attribute.String("harmostes.node.id", node.ID))

	if e.client == nil {
		return errNoDaprClient(span)
	}

	cfg, err := parseConfig[DaprPublishConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	payload := resolveTemplate(cfg.Payload, env.Inputs)

	span.SetAttributes(
		attribute.String("harmostes.dapr.pubsub", cfg.Pubsub),
		attribute.String("harmostes.dapr.topic", cfg.Topic),
		attribute.Int("harmostes.dapr.payload_len", len(payload)),
	)

	if err := e.client.Publish(ctx, cfg.Pubsub, cfg.Topic, payload); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: "dapr publish: " + err.Error()}, err
	}

	return NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"topic": cfg.Topic},
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// errNoDaprClient returns a standard error result for when a Dapr node is
// executed without a Dapr client wired.
func errNoDaprClient(span trace.Span) (NodeResult, error) {
	err := fmt.Errorf("dapr node executed without a Dapr client — wire dapr.Client in Dependencies")
	span.SetStatus(codes.Error, err.Error())
	return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
}

// resolveTemplate renders a Go text/template against node inputs. If the string
// contains no template delimiters ({{), it is returned as-is. Parse or
// execution errors return the original string unchanged.
func resolveTemplate(tmpl string, inputs map[string]NodeOutputs) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	t, err := template.New("resolve").Parse(tmpl)
	if err != nil {
		return tmpl
	}
	data := struct {
		Nodes map[string]NodeOutputs
	}{
		Nodes: inputs,
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return tmpl
	}
	return buf.String()
}
