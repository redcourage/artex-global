import type { ModelCostEstimate, ModelTokenStat } from "./types";

export type CostStatus = "complete" | "partial" | "unavailable";

export function hasReportedUsage(model: Pick<ModelTokenStat, "calls" | "usage_missing_calls">): boolean {
  return model.calls === 0 || model.usage_missing_calls !== model.calls;
}

const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 6,
});

export function formatUSD(value: number | null | undefined): string {
  if (value == null || !Number.isFinite(value)) return "—";
  if (value !== 0 && Math.abs(value) < 0.000001) return value < 0 ? "−<$0.000001" : "<$0.000001";
  return usd.format(value);
}

export function modelCostStatus(model: ModelTokenStat): CostStatus {
  if (!model.cost) return "unavailable";
  if (
    model.priced_calls == null ||
    model.unpriced_calls == null ||
    model.usage_missing_calls == null ||
    model.unpriced_calls > 0 ||
    model.usage_missing_calls > 0 ||
    model.priced_calls < model.calls
  )
    return "partial";
  return "complete";
}

const amountFields = [
  "input_usd",
  "cache_read_usd",
  "cache_write_usd",
  "output_usd",
  "total_usd",
  "without_cache_usd",
  "cache_read_savings_usd",
  "cache_write_premium_usd",
  "net_savings_usd",
] as const;

export function aggregateModelCosts(models: ModelTokenStat[]) {
  let cost: Pick<ModelCostEstimate, (typeof amountFields)[number]> | null = null;
  let partial = false;
  let coverageUnknown = false;
  let pricedCalls = 0;
  let unpricedCalls = 0;
  let usageMissingCalls = 0;
  for (const model of models) {
    partial ||= modelCostStatus(model) !== "complete";
    coverageUnknown ||= model.priced_calls == null || model.unpriced_calls == null || model.usage_missing_calls == null;
    pricedCalls += model.priced_calls ?? 0;
    unpricedCalls += model.unpriced_calls ?? 0;
    usageMissingCalls += model.usage_missing_calls ?? 0;
    if (!model.cost) continue;
    if (!cost) {
      cost = { ...model.cost };
    } else {
      for (const field of amountFields) cost[field] += model.cost[field];
    }
  }
  let status: CostStatus = "unavailable";
  if (cost) status = partial ? "partial" : "complete";
  return { cost, status, pricedCalls, unpricedCalls, usageMissingCalls, coverageUnknown };
}
