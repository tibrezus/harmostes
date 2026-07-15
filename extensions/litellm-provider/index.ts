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
 * Models are referenced as litellm/<model-id>, e.g. litellm/zai/glm-4.7.
 * The <model-id> is passed verbatim to LiteLLM's /v1/chat/completions endpoint,
 * which routes it to the correct upstream provider.
 *
 * Model discovery is dynamic: the extension fetches /v1/models at startup so
 * new models added to the proxy are available immediately without rebuilding
 * the worker image.
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

  _pi.registerProvider("litellm", {
    name: "LiteLLM Proxy",
    baseUrl: `${baseUrl}/v1`,
    apiKey: "$LITELLM_API_KEY",
    api: "openai-completions",
    authHeader: true,
    models: models.map((model) => ({
      id: model.id,
      name: model.id,
      reasoning: false,
      input: ["text" as const],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: model.max_input_tokens ?? 131072,
      maxTokens: model.max_output_tokens ?? 8192,
      compat: {
        // LiteLLM proxies upstream providers; use the broadest-compatible flags.
        supportsDeveloperRole: false,
        maxTokensField: "max_tokens",
      },
    })),
  });

  console.error(
    `[litellm-provider] registered ${models.length} model(s): ${models.map((m) => m.id).join(", ")}`,
  );
}
