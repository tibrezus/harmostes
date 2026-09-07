import assert from "node:assert/strict";
import { test } from "node:test";
import { DEFAULT_FALLBACKS, resolveFallbackChains } from "./fallbacks.ts";

test("no env keeps the default chain", () => {
  const { chains, warning } = resolveFallbackChains(undefined);
  assert.deepEqual(chains, DEFAULT_FALLBACKS);
  assert.equal(warning, undefined);
});

test("empty env keeps the default chain", () => {
  const { chains, warning } = resolveFallbackChains("  ");
  assert.deepEqual(chains, DEFAULT_FALLBACKS);
  assert.equal(warning, undefined);
});

test("valid override replaces the whole map", () => {
  const { chains, warning } = resolveFallbackChains('{"a/b": ["c/d"]}');
  assert.deepEqual(chains, { "a/b": ["c/d"] });
  assert.equal(warning, undefined);
});

test("unparsable JSON degrades loudly to the default", () => {
  const { chains, warning } = resolveFallbackChains("{not json");
  assert.deepEqual(chains, DEFAULT_FALLBACKS);
  assert.match(warning!, /not valid JSON/);
});

test("JSON array (not object) degrades to the default", () => {
  const { chains, warning } = resolveFallbackChains('["a/b"]');
  assert.deepEqual(chains, DEFAULT_FALLBACKS);
  assert.match(warning!, /not a JSON object/);
});

const malformedShapes = ['{"a/b": "c/d"}', '{"a/b": []}', '{"a/b": [null]}', '{"a/b": [42]}', '{"a/b": [""]}'];
for (const raw of malformedShapes) {
  test(`semantically-bad value ${raw} degrades to the default`, () => {
    const { chains, warning } = resolveFallbackChains(raw);
    assert.deepEqual(chains, DEFAULT_FALLBACKS);
    assert.match(warning!, /not a non-empty array/);
  });
}
import { applyChains } from "./fallbacks.ts";

const proxy = new Map([
  ["ali/anthropic/qwen3.8-flash", { max_input_tokens: 1048576, max_output_tokens: 32768 }],
  ["mtplx/qwen38-27b-optimized-quality-fp16", { max_input_tokens: 262144, max_output_tokens: 32768 }],
]);
const models = [
  { id: "ali/anthropic/qwen3.8-flash", max_input_tokens: 1048576, max_output_tokens: 32768 },
  { id: "zai/anthropic/glm-5.3-flash", max_input_tokens: 131072 },
];

test("applyChains: unchained model carries NO fallbacks key (an explicit [] overrides proxy config)", () => {
  const { annotated, wired } = applyChains(models, {}, proxy);
  const glm = annotated.find((m) => m.id === "zai/anthropic/glm-5.3-flash")!;
  assert.equal(glm.samplingParams, undefined);
  assert.equal(wired.length, 0);
});

test("applyChains: chained model gets the chain and the min-window clamp", () => {
  const { annotated, wired } = applyChains(models, {
    "ali/anthropic/qwen3.8-flash": ["mtplx/qwen38-27b-optimized-quality-fp16"],
  }, proxy);
  const flash = annotated.find((m) => m.id === "ali/anthropic/qwen3.8-flash")!;
  assert.deepEqual(flash.samplingParams, { fallbacks: ["mtplx/qwen38-27b-optimized-quality-fp16"] });
  assert.equal(flash.contextWindow, 262144); // min(1048576, 262144) — the replay must fit
  assert.match(flash.clampNote!, /fallback clamp/);
  assert.equal(wired.length, 1);
});

test("applyChains: unknown fallback ids are dropped, partial chains stay wired", () => {
  const { annotated, wired } = applyChains(models, {
    "ali/anthropic/qwen3.8-flash": ["mtplx/gone", "mtplx/qwen38-27b-optimized-quality-fp16"],
  }, proxy);
  const flash = annotated.find((m) => m.id === "ali/anthropic/qwen3.8-flash")!;
  assert.deepEqual(flash.samplingParams, { fallbacks: ["mtplx/qwen38-27b-optimized-quality-fp16"] });
  assert.deepEqual(flash.droppedIds, ["mtplx/gone"]);
  assert.equal(wired.length, 1); // r17 failure mode: wired must not read unwired
});

test("applyChains: a chain that filters to empty attaches nothing", () => {
  const { annotated, wired } = applyChains(models, {
    "ali/anthropic/qwen3.8-flash": ["mtplx/gone"],
  }, proxy);
  const flash = annotated.find((m) => m.id === "ali/anthropic/qwen3.8-flash")!;
  assert.equal(flash.samplingParams, undefined);
  assert.deepEqual(flash.droppedIds, ["mtplx/gone"]);
  assert.equal(wired.length, 0);
});
