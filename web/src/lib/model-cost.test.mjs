import { aggregateModelCosts, formatUSD, hasReportedUsage, modelCostStatus } from "./model-cost.ts";
import assert from "node:assert/strict";
import { test } from "node:test";

const cost = {
  currency: "USD",
  basis: "standard_api_equivalent",
  price_version: "2026-10-16",
  source_url: "https://openai.com/api/pricing/",
  input_usd: 0.1,
  cache_read_usd: 0.01,
  cache_write_usd: 0.04,
  output_usd: 0.2,
  total_usd: 0.35,
  without_cache_usd: 0.4,
  cache_read_savings_usd: 0.06,
  cache_write_premium_usd: 0.01,
  net_savings_usd: 0.05,
};
const row = {
  model: "example",
  calls: 2,
  input_tokens: 1000,
  output_tokens: 100,
  cache_read_tokens: 100,
  cache_write_tokens: 50,
  cost,
  priced_calls: 2,
  unpriced_calls: 0,
  usage_missing_calls: 0,
};

test("USD formatting distinguishes unknown, zero, tiny estimates and negative savings", () => {
  assert.equal(formatUSD(null), "—");
  assert.equal(formatUSD(undefined), "—");
  assert.equal(formatUSD(Number.NaN), "—");
  assert.equal(formatUSD(0), "$0.00");
  assert.equal(formatUSD(0.000123), "$0.000123");
  assert.equal(formatUSD(0.00000001), "<$0.000001");
  assert.equal(formatUSD(-0.00000001), "−<$0.000001");
  assert.equal(formatUSD(-0.125), "-$0.125");
});

test("cost coverage labels null, legacy, missing usage and partial priced calls", () => {
  assert.equal(modelCostStatus(row), "complete");
  assert.equal(modelCostStatus({ ...row, unpriced_calls: 1 }), "partial");
  assert.equal(modelCostStatus({ ...row, usage_missing_calls: 1 }), "partial");
  assert.equal(modelCostStatus({ ...row, cost: null }), "unavailable");
  assert.equal(modelCostStatus({ ...row, cost: undefined }), "unavailable");
});

test("missing usage stays distinct from reported zero usage or legacy records", () => {
  assert.equal(hasReportedUsage(row), true);
  assert.equal(hasReportedUsage({ ...row, usage_missing_calls: 2 }), false);
  assert.equal(hasReportedUsage({ ...row, usage_missing_calls: 1 }), true);
  assert.equal(hasReportedUsage({ ...row, usage_missing_calls: undefined }), true);
});

test("aggregation sums server cost fields once and excludes unknown costs from the priced subtotal", () => {
  const summary = aggregateModelCosts([
    row,
    { ...row, model: "unknown", cost: null, priced_calls: 0, unpriced_calls: 2 },
  ]);
  assert.equal(summary.status, "partial");
  assert.equal(summary.cost.total_usd, 0.35);
  assert.equal(summary.cost.without_cache_usd, 0.4);
  assert.equal(summary.cost.net_savings_usd, 0.05);
  assert.equal(summary.pricedCalls, 2);
  assert.equal(summary.unpricedCalls, 2);
});

test("aggregation keeps unknown totals separate from a real zero cost", () => {
  assert.equal(aggregateModelCosts([]).cost, null);
  assert.equal(aggregateModelCosts([{ ...row, cost: null }]).status, "unavailable");
  const zero = Object.fromEntries(
    Object.entries(cost).map(([key, value]) => [key, typeof value === "number" ? 0 : value]),
  );
  const summary = aggregateModelCosts([{ ...row, cost: zero }]);
  assert.equal(summary.status, "complete");
  assert.equal(summary.cost.total_usd, 0);
});

test("aggregation preserves negative cache savings and signals legacy coverage as unknown", () => {
  const summary = aggregateModelCosts([row, { ...row, cost: { ...cost, total_usd: 0.6, net_savings_usd: -0.2 } }]);
  assert.equal(summary.cost.total_usd, 0.95);
  assert.ok(Math.abs(summary.cost.net_savings_usd - -0.15) < 1e-12);
  const legacy = {
    model: "legacy",
    calls: 3,
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
  };
  const partial = aggregateModelCosts([row, legacy]);
  assert.equal(partial.status, "partial");
  assert.equal(partial.coverageUnknown, true);
});
