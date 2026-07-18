# Event-Driven Workflow Triggers

## Problem

Harmostes currently polls every 5 minutes to check if workflows need to run. For git-triggered workflows (platform-website, rezuscloud, rhesadox, signoz, forgejo), this means:

- 5+ minute delay between a git push and workflow execution
- Wasted controller cycles (reconciling even when nothing changed)
- No traceability of what event triggered a run

## Current Architecture

```
Controller (every 5 min)
  → Reconcile() → isDue() → createWorkerJob()
```

The `isDue()` function:
```go
func (r *WorkflowReconciler) isDue(wf *v1alpha1.Workflow) (bool, time.Duration) {
    if wf.Status.ObservedGeneration != wf.Generation {
        return true, r.PollInterval
    }
    if !wf.Status.LastRunAt.IsZero() {
        elapsed := time.Since(wf.Status.LastRunAt.Time)
        if elapsed < r.PollInterval {
            return false, r.PollInterval - elapsed
        }
    }
    return true, r.PollInterval
}
```

## Proposed Solutions

### Option 1: Webhook Trigger (Recommended)

Add a webhook endpoint that receives push events from GitHub/GitLab/Forgejo.

**Architecture:**
```
Git push event
  → webhook.k8s ( AdmissionController / HTTP handler )
  → Workflow CR trigger annotation (harmostes.dev/trigger-revision: abc123)
  → Controller reconciles immediately
```

**Implementation:**

1. Add `webhook` field to SourceSpec:
   ```go
   type SourceSpec struct {
       Kind     string            `json:"kind"`
       Repo     string            `json:"repo,omitempty"`
       Branch   string            `json:"branch,omitempty"`
       Schedule string            `json:"schedule,omitempty"`
       Topic    string            `json:"topic,omitempty"`        // Dapr pub/sub
       Webhook  *WebhookSpec      `json:"webhook,omitempty"`      // NEW
       ...
   }

   type WebhookSpec struct {
       Secret string `json:"secret"`  // HMAC secret for verification
       URL    string `json:"url"`     // Git host URL (for header verification)
   }
   ```

2. Add webhook HTTP handler to controller:
   ```go
   // POST /webhook/{workflow-name}
   // Headers: X-Hub-Signature-256 (GitHub), X-Gitlab-Token (GitLab), etc.
   // Body: push event JSON
   func (r *WorkflowReconciler) HandleWebhook(w http.ResponseWriter, req *http.Request, workflowName string) {
       // 1. Verify HMAC signature
       // 2. Extract revision from event body
       // 3. Annotate Workflow: harmostes.dev/trigger-revision=abc123
       // 4. Force requeue: r.Client.Status().Update(ctx, wf)
   }
   ```

3. Update Reconcile() to respect webhook triggers:
   ```go
   triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]
   if triggerRev != "" && triggerRev != wf.Status.LastProcessedRevision {
       // Trigger immediately
       return true, 0
   }
   ```

**Git webhook setup:**

```yaml
# GitHub (settings/hooks)
POST https://harmostes.<domain>/webhook/platform-website
Headers: Content-Type: application/json, X-Hub-Signature-256: sha256=...
Body: { "ref": "refs/heads/main", "repository": {...}, "after": "abc123..." }

# GitLab (settings/hooks)
POST https://harmostes.<domain>/webhook/platform-website
Headers: X-Gitlab-Token: <secret>
Body: { "ref": "main", "checkout_sha": "abc123...", "repository": {...} }

# Forgejo (settings/hooks)
POST https://harmostes.<domain>/webhook/platform-website
Headers: X-Forgejo-Signature: sha256=...
Body: similar to GitHub
```

**Pros:**
- Immediate triggers on push (no delay)
- Standard pattern used by CI/CD systems
- No polling overhead
- Traceability: each run linked to a specific commit

**Cons:**
- Requires exposing HTTP endpoint (via Gateway API + TLS)
- Need to manage webhook secrets per repo/host
- More complex security (HMAC verification)

---

### Option 2: Watch Flux GitRepository

Many git workflows likely have corresponding Flux GitRepository resources. Watch those for updates.

**Architecture:**
```
GitRepository (Flux) updates
  → Controller watches GitRepository CR
  → Annotates Workflow with new revision
  → Controller reconciles immediately
```

**Implementation:**

1. Add Watch() for GitRepository in SetupWithManager:
   ```go
   func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
       return ctrl.NewControllerManagedBy(mgr).
           For(&v1alpha1.Workflow{}).
           Watches(
               &sourcev1.GitRepository{},
               handler.EnqueueRequestsFromMapFunc(r.gitRepoToWorkflow),
           ).
           ...
   }

   func (r *WorkflowReconciler) gitRepoToWorkflow(ctx context.Context, obj client.Object) []reconcile.Request {
       repo := obj.(*sourcev1.GitRepository)
       var workflows v1alpha1.WorkflowList
       r.List(ctx, &workflows, client.MatchingFields{"spec.source.repo": repo.Name})
       // Return reconcile requests for each workflow watching this repo
   }
   ```

2. Reconcile GitRepository revision changes:
   ```go
   if repo.Status.Artifact != nil && repo.Status.Artifact.Revision != "" {
       // Trigger workflow if revision changed
       lastRev := wf.Status.LastProcessedRevision
       if repo.Status.Artifact.Revision != lastRev {
           return true, 0  // Trigger immediately
       }
   }
   ```

**Pros:**
- Leverages existing Flux infrastructure
- No new endpoints or secrets
- Event-driven (Flux already watches git)

**Cons:**
- Only works if using Flux GitRepository
- Some workflows might use direct git clones (non-Flux)

---

### Option 3: Dapr Event-Driven (Pub/Sub)

Extend the existing Dapr pub/sub model for inbound events.

**Architecture:**
```
Git push event
  → Webhook → Dapr pub/sub topic "git.push"
  → Workflow subscribes to "git.push:<repo>" topic
  → Dapr triggers controller
```

**Implementation:**

1. Define inbound topic pattern:
   ```yaml
   SourceSpec:
     Kind: event
     Topic: "git.push:rezuscloud/platform-website"  # inbound subscription
   ```

2. Add Dapr subscription resource:
   ```yaml
   apiVersion: dapr.io/v1alpha1
   kind: Subscription
   metadata:
     name: harmostes-platform-website
     namespace: harmostes
   spec:
     topic: git.push:rezuscloud/platform-website
     route: /webhook/platform-website
     pubsubname: harmostes-events
   ```

3. Controller needs to accept Dapr events via HTTP:
   - Similar to Option 1, but uses Dapr pub/sub for delivery

**Pros:**
- Consistent with existing outbound event model
- Decoupled (external webhook can publish to topic)
- Uses Dapr's built-in pub/sub

**Cons:**
- Requires Dapr pub/sub infrastructure
- Still needs external webhook to publish events
- More moving parts

---

### Option 4: Hybrid Approach

Combine multiple triggers:

```yaml
SourceSpec:
  Kind: git
  Repo: platform-website
  Webhook:
    Secret: xxx
    URL: https://github.com/rezuscloud/platform-website
  PollInterval: 5m  # fallback

# Or watch Flux GitRepository:
SourceSpec:
  Kind: git
  Repo: flux-source:platform-website  # special syntax
```

---

## Recommendation

**Implement Option 1 (Webhook) first**, with Option 2 (Flux watch) as an enhancement.

**Phase 1: Webhook triggers**

1. Add WebhookSpec to SourceSpec
2. Implement webhook HTTP handler
3. Update Reconcile() to respect webhook annotations
4. Add Gateway API HTTPRoute for external access
5. Update workflow CRDs to document webhook setup

**Phase 2: Flux GitRepository watch**

1. Add GitRepository watch to controller
2. Map git repo → workflow(s) via spec.source.repo field
3. Trigger on Artifact revision changes
4. Document Flux-based trigger pattern

**Phase 3: Fallback polling**

- Keep minimal polling (e.g., 15m) as safety net
- Only workflows with webhook/flux-watch disable polling

---

## Migration Path

Existing workflows keep polling. Add webhook config opt-in:

```yaml
apiVersion: harmostes.dev/v1alpha1
kind: Workflow
metadata:
  name: platform-website
spec:
  source:
    kind: git
    repo: https://github.com/rezuscloud/platform-website
    branch: master
    webhook:  # NEW: opt-in to event-driven
      secret: ${GITHUB_WEBHOOK_SECRET}  # from Secret
      url: https://github.com/rezuscloud/platform-website
```

---

## Open Questions

1. Should we support multiple triggers per workflow? (webhook + flux-watch + polling fallback)
2. How to handle branch-specific triggers? (e.g., only trigger on master, not feature branches)
3. Should we record the triggering event in WorkflowStatus? (e.g., TriggeredBy: webhook:abc123)
4. How to handle rate limiting? (throttle webhooks during rapid pushes)