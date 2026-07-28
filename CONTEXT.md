# Harmostes

Harmostes is a Kubernetes-native orchestration platform for workflows that combine deterministic operations with explicitly non-deterministic operations such as agentic reasoning. Its core model is a graph-native workflow kernel, with higher-level workflow types projected onto that graph.

## Language

**Workflow**:
A graph-native orchestration definition composed of typed nodes and edges.
_Avoid_: Pipeline, fixed loop

**Node**:
A typed unit of work in a Workflow whose execution contract is defined by the orchestration kernel.
_Avoid_: Phase, step container

**Deterministic Node**:
A Node whose outcome is expected to be reproducible from the same inputs and policies.
_Avoid_: Smart step, AI step

**Non-Deterministic Node**:
A Node whose outcome depends on interpretation, external judgment, or unstable side effects.
_Avoid_: Normal step

**Deterministic Orchestration Kernel**:
The part of Harmostes that schedules Nodes, records their inputs and outputs, evaluates rules, and advances Workflow state without making interpretive decisions itself.
_Avoid_: Smart controller, agentic orchestrator

**Execution Boundary**:
The explicit boundary where non-deterministic work is allowed to happen and must emit a durable output artifact back to the orchestration kernel.
_Avoid_: Implicit side effect

**Execution Class**:
The native way a Node is executed by Harmostes, such as a workload, a Kubernetes API operation, or pure orchestration logic.
_Avoid_: Everything is a pod

**Workload Node**:
A Node executed by launching a Kubernetes workload such as a Job or Pod.
_Avoid_: Generic node

**Kubernetes API Node**:
A Node executed directly by the orchestration kernel against the Kubernetes API for declarative intent and observed status evaluation.
_Avoid_: kubectl wrapper job, remote shell

**Pure Orchestration Node**:
A Node that changes workflow control flow or state without launching external work.
_Avoid_: Fake task

**Declarative Cluster Intent**:
A change to cluster resource state expressed through Kubernetes objects or their status evaluation rather than remote command execution.
_Avoid_: imperative shell action

**External System Binding**:
A named reference from a Workflow to a specific external system surface such as a repo, issue tracker, wiki, or webhook origin, including its host type, object identity, and access metadata.
_Avoid_: Platform

**Binding Role**:
The purpose an **External System Binding** serves in a Workflow, such as source repository, workspace repository, issue tracker, wiki, release target, or webhook origin.
_Avoid_: generic endpoint

**Surface Contract**:
The explicit kind of external surface a binding represents, rather than an inferred capability of a broader host or project.
_Avoid_: whole host binding

**Binding Authority Boundary**:
The fixed scope of external access a Workflow is allowed to use, defined by its declared **External System Bindings** or trusted internal configuration.
_Avoid_: runtime-discovered authority

**Connection Profile**:
A trusted internal configuration entry that defines how Harmostes speaks to a host type or endpoint family, including transport, auth, webhook, and API behavior.
_Avoid_: per-workflow host mechanics

**Canonical Surface Kind**:
A small kernel-recognized category of external surface, such as repository, issue tracker, wiki, review, release target, or webhook origin.
_Avoid_: plugin-private surface taxonomy

**Surface Capability**:
A specific operation a Node may request against an External System Binding, such as read, push, comment, update, or verify.
_Avoid_: implicit permission

**Capability Policy**:
The kernel-enforced rule that determines whether a Node may exercise a requested **Surface Capability** against a given binding.
_Avoid_: binding implies full access

**Canonical Orchestration History**:
The authoritative internal timeline Harmostes maintains for workflow execution, including node execution, claims, validation, promotion, and state transitions across all external surfaces.
_Avoid_: scattered external-only history

**Evidence Reference**:
A durable link from Harmostes history to an external artifact such as a commit, issue comment, wiki page, review object, or cluster resource condition.
_Avoid_: vague citation

**Implementation Attempt**:
The canonical unit of orchestration history representing Harmostes trying to achieve a specific implementation objective across runs, nodes, validations, and external side effects.
_Avoid_: single run

**Implementation Objective**:
The explicit structured change goal an **Implementation Attempt** is trying to realize, independent of how many triggers or runs are required.
_Avoid_: raw trigger event, free-form label

**Objective Kind**:
The canonical category of an **Implementation Objective**, such as documentation sync, PR review, fork sync, or deployment change.
_Avoid_: arbitrary objective string

**Objective Subject**:
The bound external or cluster object an **Implementation Objective** is acting on, consisting of one primary subject and zero or more related subjects.
_Avoid_: implicit target

**Primary Subject**:
The central bound object an **Implementation Objective** is primarily about.
_Avoid_: equal-weight target bag

**Related Subject**:
An additional bound object that participates in an **Implementation Objective** but is not its central target.
_Avoid_: hidden dependency

**Desired Outcome**:
The intended terminal result an **Implementation Objective** is trying to achieve.
_Avoid_: unspecified success

**Objective Identity**:
The stable identity key of an **Implementation Objective** used to determine whether new triggers continue an existing **Implementation Attempt** or start a new one, derived from both the subject and the targeted state or version.
_Avoid_: trigger id, subject-only identity

**Targeted State**:
The specific revision, head SHA, release, desired spec hash, or other versioned target that makes an **Implementation Objective** a distinct implementation rather than just work on the same subject.
_Avoid_: vague latest state

**Reconciliation Goal**:
The intended target state an **Implementation Attempt** is trying to realize through repeated execution and validation.
_Avoid_: event-only response

**Workflow Run**:
One execution episode of a Workflow inside an **Implementation Attempt**.
_Avoid_: the whole history

**Node Result Envelope**:
The universal structured result produced by a Node execution and consumed by the orchestration kernel for control-flow, provenance, and policy decisions.
_Avoid_: ad hoc node result

**Node Payload**:
The node-type-specific structured data nested inside a **Node Result Envelope**.
_Avoid_: top-level custom result shape

**Claim**:
A typed, reference-backed statement in a **Node Result Envelope** describing an observable external fact produced or observed by a Node execution.
_Avoid_: narrative assertion

**Reference-Backed Fact**:
An observable fact tied to durable identifiers such as a commit SHA, issue number, page path, resource name, or URL.
_Avoid_: unsupported conclusion

**Claim Trust Class**:
The authority level of a Claim, such as observed or validated, that determines whether the orchestration kernel may rely on it for authoritative decisions.
_Avoid_: all claims are equally true

**Deterministic Validation**:
A reproducible check performed by deterministic logic that confirms whether a Claim is authoritative, including the claimed external side effects when they are part of the result.
_Avoid_: trusting agent output directly

**External Side Effect**:
A claimed change on an external bound surface such as a repo, issue tracker, wiki, or review system.
_Avoid_: assumed write

**Gate**:
A validation boundary that determines whether downstream progress is allowed.
_Avoid_: Mere status check

**Gate Template**:
A reusable workflow archetype that projects a standard graph structure and policy onto a Workflow.
_Avoid_: The workflow itself

## Relationships

- A **Workflow** contains one or more **Nodes** connected by edges
- A **Workflow** may contain both **Deterministic Nodes** and **Non-Deterministic Nodes**
- The **Deterministic Orchestration Kernel** advances a **Workflow** by scheduling **Nodes** and evaluating their recorded outputs
- Every **Node** has an **Execution Class**
- An **Execution Boundary** is where a **Non-Deterministic Node** produces durable outputs for the **Deterministic Orchestration Kernel** to consume
- A **Workload Node** runs as Kubernetes compute, while a **Kubernetes API Node** acts directly on cluster resources, and a **Pure Orchestration Node** changes only workflow state or routing
- A **Kubernetes API Node** is limited to **Declarative Cluster Intent** and observed status evaluation
- Imperative remote execution against running workloads belongs to a **Workload Node**, not a **Kubernetes API Node**
- A **Workflow** may bind to multiple **External System Bindings**
- Every **External System Binding** has a **Binding Role** and a **Surface Contract**
- The set of declared **External System Bindings** defines the **Binding Authority Boundary** of a **Workflow**
- An **External System Binding** identifies the target surface, while a **Connection Profile** defines how Harmostes speaks to it
- Every **External System Binding** declares a **Canonical Surface Kind**
- A **Node** requests one or more **Surface Capabilities** against the bindings it uses
- The kernel applies **Capability Policy** before execution
- Multiple **External System Bindings** may point at related surfaces on the same host while remaining distinct bindings
- Nodes may create or observe objects within a declared binding, but may not expand the **Binding Authority Boundary** at runtime
- Harmostes maintains a **Canonical Orchestration History** across node executions and state transitions
- The primary unit in that history is an **Implementation Attempt**
- Every **Implementation Attempt** is anchored by an **Implementation Objective**
- Every **Implementation Objective** has an **Objective Kind**, an **Objective Subject**, and a **Desired Outcome**
- Every **Implementation Objective** also has an **Objective Identity**
- Every **Objective Identity** includes both the subject and the **Targeted State**
- Every **Implementation Attempt** is a **Reconciliation Goal** toward that target state
- Every **Objective Subject** contains one **Primary Subject** and may include multiple **Related Subjects**
- A single **Implementation Attempt** may contain multiple **Workflow Runs**
- New triggers continue an existing **Implementation Attempt** when the **Objective Identity** remains the same
- External artifacts are connected to that history through **Evidence References**
- Every **Node** produces a **Node Result Envelope**
- A **Node Result Envelope** contains a node-specific **Node Payload** under a universal kernel-readable structure
- A **Node Result Envelope** may contain one or more **Claims**
- Every **Claim** is a **Reference-Backed Fact** associated with the relevant **External System Binding**
- Every **Claim** has a **Claim Trust Class**
- Claims emitted by **Non-Deterministic Nodes** are not authoritative until **Deterministic Validation** confirms them
- **Deterministic Validation** must confirm both local artifacts and claimed **External Side Effects** when those side effects are part of the result
- A **Gate** constrains progress between parts of a **Workflow**
- A **Gate Template** generates or constrains a standard **Workflow** graph shape

## Example dialogue

> **Dev:** "Is Harmostes fundamentally a prepare-agent-deploy pipeline?"
> **Domain expert:** "No — a **Workflow** is fundamentally a graph. Prepare, agent, gate, and deploy are just one possible graph shape, often supplied by a **Gate Template**."
>
> **Dev:** "Can the controller itself decide things with an LLM during reconcile?"
> **Domain expert:** "No — the **Deterministic Orchestration Kernel** stays deterministic. Any interpretive work must cross an **Execution Boundary** into an explicit **Non-Deterministic Node**."
>
> **Dev:** "Does every Node become a Job?"
> **Domain expert:** "No — a **Node** has an **Execution Class**. Some run as workloads, some act directly through the Kubernetes API, and some are pure orchestration logic."
>
> **Dev:** "Can a Kubernetes API Node exec into a pod to inspect it?"
> **Domain expert:** "No — a **Kubernetes API Node** expresses **Declarative Cluster Intent** and evaluates object status. Remote command execution belongs to a **Workload Node**."
>
> **Dev:** "Is GitHub the workflow's platform?"
> **Domain expert:** "No — GitHub is an **External System Binding**. A **Workflow** may use several bindings with different **Binding Roles**, such as source repository, issue tracker, and wiki."
>
> **Dev:** "Can one GitHub binding cover repo, issues, and wiki?"
> **Domain expert:** "No — each binding carries a **Surface Contract**. Related surfaces may live on the same host, but they remain distinct **External System Bindings**."
>
> **Dev:** "Can a node discover a new external target at runtime and start using it?"
> **Domain expert:** "No — the **Binding Authority Boundary** is fixed by declared bindings or trusted internal configuration. Nodes may create objects inside a binding, but they may not create new bindings at runtime."
>
> **Dev:** "Should every Workflow describe GitHub or GitLab API quirks inline?"
> **Domain expert:** "No — the Workflow declares the target **External System Binding**, while a trusted **Connection Profile** defines how Harmostes talks to that host family."
>
> **Dev:** "Does the kernel understand what kind of external surface a binding is?"
> **Domain expert:** "Yes — every binding declares a **Canonical Surface Kind** from a small shared kernel vocabulary, even if host-specific details live deeper in node payloads or connection profiles."
>
> **Dev:** "If a node references a wiki binding, can it do anything the wiki surface supports?"
> **Domain expert:** "No — the node must request explicit **Surface Capabilities**, and the kernel enforces **Capability Policy** before execution."
>
> **Dev:** "Where does the authoritative history of a workflow live if it touches commits, issues, wikis, and cluster resources?"
> **Domain expert:** "In Harmostes’ **Canonical Orchestration History**. External systems keep their native artifacts, but Harmostes keeps the authoritative cross-surface execution timeline through **Evidence References**."
>
> **Dev:** "Is the main history object just a workflow run?"
> **Domain expert:** "No — the main history object is an **Implementation Attempt**. A **Workflow Run** is only one execution episode inside that larger attempt."
>
> **Dev:** "What anchors an Implementation Attempt? The webhook or the schedule tick?"
> **Domain expert:** "No — an **Implementation Attempt** is anchored by an **Implementation Objective**. Triggers and runs are only execution episodes used to pursue that objective."
>
> **Dev:** "Is the objective just a free-text label like 'update docs'?"
> **Domain expert:** "No — an **Implementation Objective** has a small canonical structure, including an **Objective Kind**, an **Objective Subject**, and a **Desired Outcome**."
>
> **Dev:** "Can the objective subject span repo, wiki, and issue surfaces at once?"
> **Domain expert:** "Yes — but not as an unstructured bag. The **Objective Subject** has one **Primary Subject** and may include additional **Related Subjects**."
>
> **Dev:** "If the same objective re-triggers, do we start a new attempt every time?"
> **Domain expert:** "No — new triggers continue the existing **Implementation Attempt** as long as the **Objective Identity** remains the same."
>
> **Dev:** "Is the identity just the subject, like a repo or PR number?"
> **Domain expert:** "No — **Objective Identity** includes the **Targeted State** too, so different revisions, release targets, or desired specs become distinct implementation attempts."
>
> **Dev:** "Is an implementation attempt just a reaction to a webhook?"
> **Domain expert:** "No — an **Implementation Attempt** is a **Reconciliation Goal** toward a **Targeted State**. Triggers are only wake-ups that may cause the reconciliation to run."
>
> **Dev:** "Can each node return any JSON shape it wants?"
> **Domain expert:** "No — every **Node** returns a **Node Result Envelope**. Node-specific details live inside the **Node Payload**, but the kernel always reads the same top-level contract."
>
> **Dev:** "Can an agent claim that the docs are correct now?"
> **Domain expert:** "Not as a trusted kernel fact. A **Claim** must be a **Reference-Backed Fact**, such as a commit, issue comment, page update, or resource condition tied to durable identifiers."
>
> **Dev:** "If an agent reports a commit SHA or wiki update, can the kernel trust it immediately?"
> **Domain expert:** "No — claims from **Non-Deterministic Nodes** remain untrusted until **Deterministic Validation** promotes their **Claim Trust Class** to authoritative."
>
> **Dev:** "Is it enough that the local build passes if the node also claimed it posted to an issue tracker?"
> **Domain expert:** "No — **Deterministic Validation** must also confirm the claimed **External Side Effect** on the relevant **External System Binding**."

## Flagged ambiguities

- "workflow" and "pipeline" were both used for the main orchestration model — resolved: **Workflow** is the canonical term.
- The repo described Harmostes as a fixed phase loop while the implementation evolved toward a graph model — resolved: the canonical model is **graph-native**, and fixed loops are templates over graphs.
- It was unclear whether non-determinism belonged in the controller or in nodes — resolved: non-determinism is allowed only at explicit **Execution Boundaries** inside **Non-Deterministic Nodes**.
- It was unclear whether all execution should be pod-backed — resolved: Harmostes supports multiple **Execution Classes**, and Kubernetes API operations are first-class execution modes.
- It was unclear whether Kubernetes-native execution included imperative pod access — resolved: **Kubernetes API Nodes** are restricted to **Declarative Cluster Intent** and status evaluation, not remote execution.
- "platform" was being used for remote systems like GitHub or GitLab — resolved: the canonical term is **External System Binding**, and a **Workflow** may carry multiple bindings with explicit **Binding Roles**.
- It was unclear whether a binding referred to an entire host/project or a specific external surface — resolved: bindings are **surface-specific** and carry an explicit **Surface Contract**.
- It was unclear whether nodes could expand external authority at runtime — resolved: the **Binding Authority Boundary** is fixed up front, and runtime may create objects only within declared bindings.
- It was unclear whether host/API mechanics belonged in every Workflow — resolved: those mechanics live in trusted **Connection Profiles**, while Workflows declare only the relevant **External System Bindings**.
- It was unclear whether the kernel should understand external surface categories or treat them as opaque — resolved: bindings declare a kernel-recognized **Canonical Surface Kind** from a small shared vocabulary.
- It was unclear whether binding presence implied blanket authority — resolved: nodes request explicit **Surface Capabilities**, and the kernel enforces **Capability Policy** before execution.
- It was unclear whether workflow history should be inferred ad hoc from external systems — resolved: Harmostes maintains the **Canonical Orchestration History**, with external artifacts linked through **Evidence References**.
- It was unclear whether history should be centered on runs or on the larger change process — resolved: the canonical history unit is an **Implementation Attempt**, and **Workflow Runs** are subordinate execution episodes.
- It was unclear whether an **Implementation Attempt** was anchored by a trigger or by the intended change — resolved: it is anchored by an explicit **Implementation Objective**.
- It was unclear whether an **Implementation Objective** was free-form or structured — resolved: it has a small canonical structure with **Objective Kind**, **Objective Subject**, and **Desired Outcome**.
- It was unclear whether the **Objective Subject** could span multiple bound objects — resolved: it may be composite, but it must have one **Primary Subject** and explicit **Related Subjects**.
- It was unclear whether repeated triggers should fragment the same implementation story — resolved: an **Implementation Attempt** continues while the **Objective Identity** remains the same.
- It was unclear whether **Objective Identity** should be subject-only or include the intended versioned target — resolved: identity includes both subject and **Targeted State**.
- It was unclear whether an Implementation Attempt was fundamentally event-driven or reconciliation-driven — resolved: it is a **Reconciliation Goal** toward a **Targeted State**.
- It was unclear whether each node could define its own top-level result shape — resolved: all nodes emit a universal **Node Result Envelope** with node-specific detail in a nested **Node Payload**.
- It was unclear whether claims were trusted free-form conclusions or observable facts — resolved: **Claims** are typed **Reference-Backed Facts**, not narrative assertions.
- It was unclear whether claims from non-deterministic execution were authoritative on their own — resolved: claims from **Non-Deterministic Nodes** always require **Deterministic Validation** before the kernel may trust them authoritatively.
- It was unclear whether validation covered only local artifacts or also external effects — resolved: **Deterministic Validation** must confirm claimed **External Side Effects** on the relevant binding as well.
