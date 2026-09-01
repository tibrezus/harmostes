// Package fixture seeds a deterministic synthetic world through the same
// construction path production uses (ui.New over a controller-runtime
// client.Client). It powers three consumers at once — `harmostes-ui -fixture`
// local development, the goquery component tests, and (later) the Playwright
// E2E target — so all three exercise identical data. Milestone ⑤ of #290.
// The world is deliberately synthetic: nothing here copies a real
// repository's workflow spec.
package fixture

import (
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrl "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/ui"
)

// DevUser is the identity the fixture world belongs to. `harmostes-ui
// -fixture` defaults the dev-user bypass to it when unset; tests send it as
// the X-Harmostes-Dev-User header.
const DevUser = "fixture-user"

// base is the fixture clock origin: one fixed morning, so every timestamp in
// the world is deterministic across runs.
var base = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

// t offsets base by minutes+seconds and returns a fresh metav1.Time.
func t(min, sec int) metav1.Time {
	return metav1.NewTime(base.Add(time.Duration(min)*time.Minute + time.Duration(sec)*time.Second))
}

// worldYAML declares the fixture workflows as YAML — the same shape an
// operator would check into k8s-config — so the file doubles as tutorial
// material for the Workflow CR.
//
//go:embed world.yaml
var worldYAML []byte

// Objects returns every non-Attempt object in the fixture world (the
// Workflows), parsed from the embedded YAML and retargeted to namespace.
func Objects(namespace string) ([]ctrlclient.Object, error) {
	docs := strings.Split(string(worldYAML), "\n---\n")
	var objs []ctrlclient.Object
	for i, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var wf v1alpha1.Workflow
		if err := yaml.Unmarshal([]byte(doc), &wf); err != nil {
			return nil, fmt.Errorf("fixture world doc %d: %w", i, err)
		}
		wf.Namespace = namespace
		if wf.Labels == nil {
			wf.Labels = map[string]string{}
		}
		wf.Labels[v1alpha1.OwnerLabel] = DevUser
		objs = append(objs, &wf)
	}
	return objs, nil
}

// envelope builds a node-result envelope with a measured duration (the
// timing waterfall's input).
func envelope(nodeID, status string, at metav1.Time, durationSec int64) v1alpha1.NodeResultEnvelope {
	return v1alpha1.NodeResultEnvelope{
		NodeID:     nodeID,
		Status:     status,
		ProducedAt: at,
		DurationMs: durationSec * 1000,
	}
}

// prReviewAttempt builds a review-class attempt skeleton against the
// pr-review-demo workflow.
func prReviewAttempt(namespace, name, pr string, created metav1.Time) *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			Labels:            map[string]string{v1alpha1.OwnerLabel: DevUser},
			CreationTimestamp: created,
		},
		Spec: v1alpha1.AttemptSpec{
			Objective: v1alpha1.ObjectiveSpec{
				Kind:           v1alpha1.ObjectiveKindPRReview,
				PrimarySubject: v1alpha1.Subject{Binding: "github", Object: pr},
			},
			WorkflowRef: namespace + "/pr-review-demo",
			Owner:       DevUser,
		},
	}
}

// mergeSyncAttempt builds a deterministic (merge-sync) attempt skeleton.
func mergeSyncAttempt(namespace, name string, created metav1.Time) *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			Labels:            map[string]string{v1alpha1.OwnerLabel: DevUser},
			CreationTimestamp: created,
		},
		Spec: v1alpha1.AttemptSpec{
			Objective: v1alpha1.ObjectiveSpec{
				Kind:           "merge-sync",
				PrimarySubject: v1alpha1.Subject{Binding: "github", Object: "demo-rezuscloud/harmostes"},
			},
			WorkflowRef: namespace + "/merge-sync-demo",
			Owner:       DevUser,
		},
	}
}

// Attempts returns the fixture attempts. Three states, one narrative:
//
//  1. pr-review-demo / demo-rezuscloud-harmostes#42 — terminal (validated),
//     full ledger including the 13m agent node, so the run graph and timing
//     waterfall render every node state.
//  2. pr-review-demo / demo-rezuscloud-harmostes#43 — mid-flight: prepare
//     has an envelope and the agent run is in flight, so the live position
//     lands on the agent node.
//  3. merge-sync-demo — superseded (a newer targeted state replaced it),
//     exercising the fourth terminal phase.
func Attempts(namespace string) ([]ctrlclient.Object, error) {
	// --- 1. terminal review attempt -------------------------------------
	terminal := prReviewAttempt(namespace, "attempt-pr-review-demo-42a1", "demo-rezuscloud/harmostes#42", t(0, 0))
	terminal.Status.Phase = v1alpha1.AttemptPhaseValidated
	terminal.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(0, 15), 5),
		envelope("agent", "ok", t(13, 25), 780), // the 13m agent node
		envelope("gate", "ok", t(14, 10), 40),
		envelope("deploy", "skipped", t(14, 15), 0),
	}
	terminal.Status.Runs = []v1alpha1.RunRecord{
		{Name: "pr-review-demo-42a1-prepare", StartedAt: t(0, 5), EndedAt: t(0, 15), Phase: "succeeded"},
		{Name: "pr-review-demo-42a1-agent", StartedAt: t(0, 20), EndedAt: t(13, 25), Phase: "succeeded"},
		{Name: "pr-review-demo-42a1-gate", StartedAt: t(13, 30), EndedAt: t(14, 10), Phase: "succeeded"},
	}

	// --- 2. mid-flight review attempt -----------------------------------
	running := prReviewAttempt(namespace, "attempt-pr-review-demo-43c2", "demo-rezuscloud/harmostes#43", t(30, 0))
	running.Status.Phase = v1alpha1.AttemptPhaseReconciling
	running.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(30, 12), 4),
	}
	running.Status.Runs = []v1alpha1.RunRecord{
		{Name: "pr-review-demo-43c2-prepare", StartedAt: t(30, 5), EndedAt: t(30, 12), Phase: "succeeded"},
		{Name: "pr-review-demo-43c2-agent", StartedAt: t(30, 20), Phase: "running"}, // no EndedAt: in flight
	}

	// --- 3. superseded merge-sync attempt -------------------------------
	superseded := mergeSyncAttempt(namespace, "attempt-merge-sync-demo-e5f6", t(60, 0))
	superseded.Status.Phase = v1alpha1.AttemptPhaseSuperseded
	superseded.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(61, 0), 6),
		envelope("deploy", "ok", t(62, 30), 55),
	}
	superseded.Status.Runs = []v1alpha1.RunRecord{
		{Name: "merge-sync-demo-e5f6-prepare", StartedAt: t(60, 50), EndedAt: t(61, 0), Phase: "succeeded"},
		{Name: "merge-sync-demo-e5f6-deploy", StartedAt: t(61, 30), EndedAt: t(62, 30), Phase: "succeeded"},
	}

	return []ctrlclient.Object{terminal, running, superseded}, nil
}

// Scheme returns the full API scheme (v1alpha1 + core types) the fake
// client needs — the same scheme production uses.
func Scheme() *runtime.Scheme {
	return k8s.Scheme()
}

// DevIdentity injects the fixture dev user into requests that carry no
// explicit identity — the zero-setup contract behind `harmostes-ui -fixture`
// (no Authentik, no headers). Wrap Routes() with it in the binary; tests
// wrap it to pin the contract.
func DevIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Authentik-Username") == "" && r.Header.Get("X-Harmostes-Dev-User") == "" {
			r.Header.Set("X-Harmostes-Dev-User", DevUser)
		}
		next.ServeHTTP(w, r)
	})
}

// NewServer constructs a ui.Server over an in-memory seeded world: the fake
// controller-runtime client carries the fixture objects, the fake clientset
// backs pod-log reads (no pods — log streaming degrades gracefully). DAPR
// stays unwired; the dapr event endpoint still mutates the in-memory world,
// which is exactly what E2E event-injection tests will use.
func NewServer(namespace string, logger *slog.Logger) (*ui.Server, error) {
	scheme := Scheme()

	objs, err := Objects(namespace)
	if err != nil {
		return nil, fmt.Errorf("fixture objects: %w", err)
	}
	atts, err := Attempts(namespace)
	if err != nil {
		return nil, fmt.Errorf("fixture attempts: %w", err)
	}
	var runtimeObjs []runtime.Object
	for _, o := range append(objs, atts...) {
		runtimeObjs = append(runtimeObjs, o.(runtime.Object))
	}

	k8sClient := fakectrl.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(runtimeObjs...).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		Build()

	// The clientset is cluster-scoped by design (client-go fakes have no
	// namespace option in v0.31): the namespace reaches it per call —
	// makeLogFetchFunc does Pods(namespace).GetLogs — so the seam stays
	// exactly as production's. No pods are seeded; log streaming degrades.
	var kubeClient kubernetes.Interface = fake.NewSimpleClientset()

	return ui.New(k8sClient, namespace, logger, kubeClient, nil)
}
