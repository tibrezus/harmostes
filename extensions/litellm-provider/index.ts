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
 * chain: ali/anthropic/qwen3.8-flash → mtplx/qwen38-27b-optimized-quality-fp16
 * (both live on the proxy). Override with LITELLM_FALLBACKS, a JSON object
 * mapping model id → array of fallback ids — resolved by fallbacks.ts, which
 * degrades to the default chain on any semantically-bad value. NOTE the
 * naming boundary: on the proxy, ids are BARE group names (mtplx/...);
 * harmostes-side model strings carry the litellm/ provider prefix
 * (litellm/mtplx/...).
 *
 * Two honest limits (r16-review): (1) LITELLM_FALLBACKS is read in the
 * worker IMAGE — overriding it means the worker Deployment's env, not a
 * per-Job knob. (2) A run that failed over is indistinguishable from a
 * healthy run here (same --model string); attribute via the proxy's router
 * logs, which record the serving group per request. Chained models register
 * min(primary, fallback) context/output windows so the post-failover replay
 * fits the fallback group's smaller window (mtplx 262144 < flash 1048576).
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { resolveFallbackChains } from "./fallbacks.ts";

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

  _pi.registerProvider("litellm", {
    name: "LiteLLM Proxy",
    baseUrl: `${baseUrl}/v1`,
    apiKey: "$LITELLM_API_KEY",
    api: "openai-completions",
    authHeader: true,
    models: models.map((model) => {
      // A fallback id that is not a discovered proxy group would make the
      // router's failover attempt fail too — drop it and warn (r17 review:
      // the clamp only worked for ids present in /v1/models, and the log
      // reported unwired chains as wired).
      const chain = (fallbacks[model.id] ?? []).filter((id) => {
        if (byId.has(id)) return true;
        console.error(`[litellm-provider] WARNING: fallback "${id}" (for ${model.id}) is not a known proxy group — dropped`);
        return false;
      });
      // Conservative windows for chained models (r16-review pillar 6):
      // after a failover LiteLLM replays the SAME payload against the
      // fallback group — if the fallback's window is smaller, the replay
      // 400s and the stream dies anyway. pi cannot see the failover, so
      // the client registers min(primary, fallback) and compacts early.
      // Live: flash primary 1048576 vs mtplx 262144 → chained models
      // register 262144 and pi compacts before the proxy ever replays.
      let contextWindow = model.max_input_tokens ?? 131072;
      let maxTokens = model.max_output_tokens ?? 8192;
      for (const id of chain ?? []) {
        const fb = byId.get(id);
        if (fb?.max_input_tokens) contextWindow = Math.min(contextWindow, fb.max_input_tokens);
        if (fb?.max_output_tokens) maxTokens = Math.min(maxTokens, fb.max_output_tokens);
      }
      return {
        id: model.id,
        name: model.id,
        reasoning: false,
        input: ["text" as const],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow,
        maxTokens,
        // LiteLLM's request-level failover: when this model group fails, the
        // proxy retries the chain server-side and the stream never breaks.
        samplingParams: chain ? { fallbacks: chain } : undefined,
        compat: {
          // LiteLLM proxies upstream providers; use the broadest-compatible flags.
          supportsDeveloperRole: false,
          maxTokensField: "max_tokens",
        },
      };
    }),
  });

  // Honest wiring log: report the chains actually ATTACHED (primary known
  // AND every fallback id known); a key that matched nothing is a warning
  // (misspelled id or a renamed proxy group — the protection silently
  // absent, r16-review pillar 7; unwired-but-logged, r17-review).
  const wired: string[] = [];
  for (const [model, chain] of Object.entries(fallbacks)) {
    if (!byId.has(model)) {
      console.error(`[litellm-provider] WARNING: fallback chain for "${model}" matched no registered model — not wired`);
      continue;
    }
    if (chain.some((id) => !byId.has(id))) continue; // per-id warnings above
    if (chain.length > 0) wired.push(`${model} → ${chain.join(", ")}`);
  }
  console.error(
    `[litellm-provider] registered ${models.length} model(s): ${models.map((m) => m.id).join(", ")}` +
      (wired.length ? ` | fallbacks wired: ${wired.join("; ")}` : " | no fallbacks wired"),
  );
}
