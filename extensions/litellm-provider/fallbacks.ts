/**
 * Fallback-chain resolution for the litellm-provider extension (#358).
 *
 * Pure and table-tested (the extension body only wires the result): the
 * contract is "a semantically-bad override degrades to the DEFAULT chain,
 * loudly" — never a crash, never a silently-wrong chain (r16-style review
 * of #359: valid JSON with a wrong shape is the accidental-value risk).
 */

export const DEFAULT_FALLBACKS: Record<string, string[]> = {
  "ali/anthropic/qwen3.8-flash": ["mtplx/qwen38-27b-optimized-quality-fp16"],
};

/**
 * Parse a LITELLM_FALLBACKS override: a JSON object whose keys are model ids
 * and whose values are NON-EMPTY ARRAYS of non-empty string model ids.
 * Anything else — unparsable JSON, non-object, non-array values, empty
 * arrays, non-string members — degrades to the default with a reason string
 * (logged by the caller).
 */
export function resolveFallbackChains(
  raw: string | undefined,
): { chains: Record<string, string[]>; warning?: string } {
  if (raw === undefined || raw.trim() === "") {
    return { chains: DEFAULT_FALLBACKS };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { chains: DEFAULT_FALLBACKS, warning: "LITELLM_FALLBACKS is not valid JSON — keeping default chains" };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { chains: DEFAULT_FALLBACKS, warning: "LITELLM_FALLBACKS is not a JSON object — keeping default chains" };
  }
  const chains: Record<string, string[]> = {};
  for (const [model, chain] of Object.entries(parsed as Record<string, unknown>)) {
    if (!Array.isArray(chain) || chain.length === 0 || !chain.every((id) => typeof id === "string" && id.trim() !== "")) {
      return {
        chains: DEFAULT_FALLBACKS,
        warning: `LITELLM_FALLBACKS["${model}"] is not a non-empty array of model ids — keeping default chains`,
      };
    }
    chains[model] = chain as string[];
  }
  return { chains };
}
