/**
 * LiteLLM Proxy provider extension for pi.
 *
 * Registers all models exposed by a LiteLLM proxy as the "litellm" provider.
 * LiteLLM is an OpenAI-compatible router/gateway, so every model uses the
 * openai-completions streaming API.
 *
 * Required env vars (injected by the harmostes controller from the
 * harmostes-litellm-token secret):
 *
 *   LITELLM_URL      e.g. https://litellm.example.com
 *   LITELLM_API_KEY  the proxy's master key or virtual key
 *
 * Models are referenced as litellm/<model-id>, e.g. litellm/zai/anthropic/glm-5.3-flash.
 * The <model-id> is passed verbatim to LiteLLM's /v1/chat/completions endpoint,
 * which routes it to the correct upstream provider.
 *
 * Model discovery is dynamic: the extension fetches /v1/models at startup so
 * new models added to the proxy are available immediately without rebuilding
 * the worker image.
 *
 * Fallbacks (#358): models with a configured fallback chain carry LiteLLM's
 * request-level `fallbacks` param (via Model.samplingParams) — when the
 * primary model group fails mid-run, the proxy's router fails over to the
 * fallback group and the agent's stream continues instead of dying. Default
 * chain: mtplx/qwen38-27b-optimized-speed-fp16 → ali/anthropic/qwen3.8-flash
 * (both live on the proxy; the platform decision, #363). Override with
 * LITELLM_FALLBACKS, a JSON object
 * mapping model id → array of fallback ids — resolved by fallbacks.ts, which
 * degrades to the default chain on any semantically-bad value. NOTE the
 * naming boundary: on the proxy, ids are BARE group names (mtplx/...);
 * harmostes-side model strings carry the litellm/ provider prefix
 * (litellm/mtplx/...).
 *
 * Honest limits (r16/r17/r18 reviews — the canonical statement; the code
 * points here instead of restating it): (1) LITELLM_FALLBACKS is delivered
 * to agents via jobEnvAllowlist (worker/dispatch.go) from the pool pod's
 * env — set it on the worker Deployment. `LITELLM_FALLBACKS='{}'` DISABLES
 * all chains (the off-switch); unset keeps the default. (2) A run that
 * failed over is indistinguishable from a healthy run here (same --model
 * string); attribute via the proxy's router logs, which record the serving
 * group per request. (3) Chained models register min(primary, fallback)
 * windows so the post-failover replay fits the fallback group — a real
 * capacity cost on every healthy run, taken for correctness — pi compacts
 * at window−reserve, so a chain whose fallback has a SMALLER window pulls
 * the primary's budget down (the old flash→speed chain compacted ~4×
 * early, 1 MiB→256 KiB). The current default has no such cost: speed
 * (256 KiB) → flash (1 MiB) registers min = speed's own window.
 * The alternative, clamping only the fallback and letting the first
 * over-long replay 400, was considered and rejected: it trades a clean
 * early compaction for a dead stream exactly when the primary is already
 * down. (4) The clamp
 * is not transitive: a proxy-side chain hanging off the fallback group is
 * invisible here. (5) A failover moves the whole review context to a
 * DIFFERENT upstream group — chains are Deployment-env-settable, so the
 * trust boundary for review payloads is whoever can edit that Deployment.
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { applyChains, resolveFallbackChains } from "./fallbacks.ts";

export default async function (_pi: ExtensionAPI) {
  const rawUrl = process.env.LITELLM_URL;
  const apiKey = process.env.LITELLM_API_KEY;

  if (!rawUrl) {
    console.error("[litellm-provider] LITELLM_URL not set — provider not registered");
    return;
  }
  if (!apiKey) {
    console.error("[litellm-provider] LITELLM_API_KEY not set — provider not registered");
    return;
  }

  // Normalise: strip trailing slash for clean URL composition.
  const baseUrl = rawUrl.replace(/\/+$/, "");

  let models: Array<{ id: string; max_input_tokens?: number; max_output_tokens?: number }>;
  try {
    const resp = await fetch(`${baseUrl}/v1/models`, {
      headers: { Authorization: `Bearer ${apiKey}` },
    });
    if (!resp.ok) {
      console.error(`[litellm-provider] /v1/models returned ${resp.status} ${resp.statusText}`);
      return;
    }
    const payload = (await resp.json()) as {
      data: Array<{ id: string; max_input_tokens?: number; max_output_tokens?: number }>;
    };
    models = payload.data ?? [];
  } catch (err) {
    console.error(`[litellm-provider] failed to fetch models from ${baseUrl}/v1/models: ${err}`);
    return;
  }

  if (models.length === 0) {
    console.error("[litellm-provider] proxy returned 0 models — provider not registered");
    return;
  }

  // Fallback chains (#358): model id → LiteLLM request-level `fallbacks`.
  // Resolution/validation is pure and table-tested (fallbacks.ts); a
  // semantically-bad LITELLM_FALLBACKS degrades to the default chain loudly.
  // NOTE: the env is read in the worker IMAGE (piargs loads this extension for
  // every agent) — reaching the override from a deployment means setting it on
  // the worker Deployment/env template, not per-Job.
  const { chains: fallbacks, warning } = resolveFallbackChains(process.env.LITELLM_FALLBACKS);
  if (warning) {
    console.error(`[litellm-provider] ${warning}`);
  }
  const byId = new Map(models.map((m) => [m.id, m]));

  const { wired, annotated } = applyChains(models, fallbacks, byId);

  _pi.registerProvider("litellm", {
    name: "LiteLLM Proxy",
    baseUrl: `${baseUrl}/v1`,
    apiKey: "$LITELLM_API_KEY",
    api: "openai-completions",
    authHeader: true,
    // All chain math is pure and table-tested (fallbacks.ts:applyChains) —
    // the wiring below only translates the result into provider config.
    models: annotated.map((model) => ({
      id: model.id,
      name: model.id,
      reasoning: false,
      input: ["text" as const],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      // LiteLLM's request-level failover: when this model group fails, the
      // proxy retries the chain server-side and the stream never breaks.
      // ABSENT on unchained models — an explicit `fallbacks: []` would
      // override proxy-configured fallbacks with "none", fleet-wide
      // (r17-review P4.1: observed live via a stubbed /v1/models).
      samplingParams: model.samplingParams,
      compat: {
        // LiteLLM proxies upstream providers; use the broadest-compatible flags.
        supportsDeveloperRole: false,
        maxTokensField: "max_tokens",
      },
    })),
  });

  for (const model of annotated) {
    for (const id of model.droppedIds) {
      console.error(`[litellm-provider] WARNING: fallback "${id}" (for ${model.id}) is not a known proxy group — dropped`);
    }
    if (model.clampNote) {
      console.error(`[litellm-provider] ${model.id}: ${model.clampNote}`);
    }
  }
  // Honest wiring log: applyChains reports the chains actually ATTACHED —
  // fully-resolved (primary known, every fallback id a discovered group)
  // and non-empty. Anything else is "no fallbacks wired" for that model.
  console.error(
    `[litellm-provider] registered ${models.length} model(s): ${models.map((m) => m.id).join(", ")}` +
      (wired.length ? ` | fallbacks wired: ${wired.join("; ")}` : " | no fallbacks wired"),
  );
}
