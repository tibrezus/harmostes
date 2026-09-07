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
 * mapping model id → array of fallback ids. NOTE the naming boundary: on the
 * proxy, ids are BARE group names (mtplx/...); harmostes-side model strings
 * carry the litellm/ provider prefix (litellm/mtplx/...).
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

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
  // Default protects the review fleet's primary; LITELLM_FALLBACKS overrides
  // the whole map (invalid JSON kills the override loudly, keeping the default).
  const defaultFallbacks: Record<string, string[]> = {
    "ali/anthropic/qwen3.8-flash": ["mtplx/qwen38-27b-optimized-quality-fp16"],
  };
  let fallbacks = defaultFallbacks;
  if (process.env.LITELLM_FALLBACKS) {
    try {
      fallbacks = JSON.parse(process.env.LITELLM_FALLBACKS) as Record<string, string[]>;
    } catch {
      console.error("[litellm-provider] LITELLM_FALLBACKS is not valid JSON — keeping default chains");
    }
  }

  _pi.registerProvider("litellm", {
    name: "LiteLLM Proxy",
    baseUrl: `${baseUrl}/v1`,
    apiKey: "$LITELLM_API_KEY",
    api: "openai-completions",
    authHeader: true,
    models: models.map((model) => {
      const chain = fallbacks[model.id];
      return {
        id: model.id,
        name: model.id,
        reasoning: false,
        input: ["text" as const],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: model.max_input_tokens ?? 131072,
        maxTokens: model.max_output_tokens ?? 8192,
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

  const wired = Object.entries(fallbacks)
    .map(([m, chain]) => `${m} → ${chain.join(", ")}`)
    .join("; ");
  console.error(
    `[litellm-provider] registered ${models.length} model(s): ${models.map((m) => m.id).join(", ")}` +
      (wired ? ` | fallbacks: ${wired}` : ""),
  );
}
