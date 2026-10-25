{{/*
Configuration contract (ADR-0011 Amendment — wiki: Configuration Contract).

Decision fields are the deployment's; the chart ships NO decisions. Each
required/fail below stops the render with the parameter's documentation when
the deployment did not decide. The contract map itself is kernel LOGIC: it
declares WHICH templates carry decision fields and WHICH fields — the values
come from the GitOps HelmRelease (k8s-config) only.

`models` is key-required but may be [] (day model always); `toolBudget` may
be 0 (uncapped; turn-budget v3 manages review attempts) — Helm `required`
fails only on empty/nil, which is exactly the contract.
*/}}
{{- define "harmostes.requireDecisions" -}}
{{- $wf := .wf -}}
{{- $contract := dict
      "pr-review" (list "model" "models" "thinking" "timeout" "toolBudget" "maxFixes")
      "wiki-lint" (list "maxFixes") -}}
{{- range $name, $fields := $contract -}}
{{- $tpl := index $wf $name | default dict -}}
{{- if $tpl -}}
{{- $agent := index $tpl "agent" | default dict -}}
{{- range $field := $fields -}}
{{- $doc := printf "workflowTemplates.%s.agent.%s is a deployment decision (ADR-0011 Amendment — wiki: Configuration Contract). Set values.workflowTemplates.%s.agent.%s in the GitOps HelmRelease. " $name $field $name $field -}}
{{- if eq $field "model" -}}
{{- $doc = printf "%sThe agent's primary model, litellm/-prefixed." $doc -}}
{{- else if eq $field "models" -}}
{{- $doc = printf "%sTime-windowed model overrides: a LIST (each entry: model, start, end, tz); [] = day model always." $doc -}}
{{- else if eq $field "thinking" -}}
{{- $doc = printf "%sReasoning-depth declaration and sampling-intent signal (e.g. high)." $doc -}}
{{- else if eq $field "timeout" -}}
{{- $doc = printf "%sAttempt wall clock in seconds — the wall the budget machinery assumes." $doc -}}
{{- else if eq $field "toolBudget" -}}
{{- $doc = printf "%sPer-attempt tool-call cap; 0 = uncapped." $doc -}}
{{- else if eq $field "maxFixes" -}}
{{- $doc = printf "%sFix-round bound for the gate loop." $doc -}}
{{- end -}}
{{- if eq $field "models" -}}
{{- if not (hasKey $agent "models") -}}
{{- fail (printf "%sThe key itself is mandatory (an empty list is valid)." $doc) -}}
{{- end -}}
{{- else -}}
{{- $_ := required $doc (index $agent $field) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
