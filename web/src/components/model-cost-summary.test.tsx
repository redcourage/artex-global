import "@testing-library/jest-dom/vitest";

import { cleanup, render, screen, within } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, describe, expect, it } from "vitest";

import type { ModelTokenStat } from "@/lib/types";

import messages from "../../messages/en.json";
import { ModelCostSummary } from "./model-cost-summary";

afterEach(cleanup);

const row = {
  model: "priced-model",
  calls: 2,
  input_tokens: 1000,
  output_tokens: 100,
  cache_read_tokens: 100,
  cache_write_tokens: 50,
  priced_calls: 2,
  unpriced_calls: 0,
  usage_missing_calls: 0,
  usage_sources: ["codex_subscription"],
  cost: {
    currency: "USD",
    basis: "standard_api_equivalent",
    price_version: "2026-10-16",
    source_url: "https://openai.com/api/pricing/",
    input_usd: 0.1,
    cache_read_usd: 0.01,
    cache_write_usd: 0.04,
    output_usd: 0.2,
    total_usd: 0.35,
    without_cache_usd: 0.3,
    cache_read_savings_usd: 0.01,
    cache_write_premium_usd: 0.06,
    net_savings_usd: -0.05,
  },
} satisfies ModelTokenStat;

function show(models: ModelTokenStat[]) {
  render(
    <NextIntlClientProvider locale="en" messages={messages}>
      <ModelCostSummary models={models} />
    </NextIntlClientProvider>,
  );
}

describe("ModelCostSummary", () => {
  it("labels partial totals and unknown model costs without inventing zero charges", () => {
    show([row, { ...row, model: "unknown-model", cost: null, priced_calls: 0, unpriced_calls: 2 }]);
    expect(screen.getByText("Partial priced subtotal")).toBeInTheDocument();
    const unknown = screen.getByRole("row", { name: /unknown-model/ });
    expect(within(unknown).getByText("Estimate unavailable")).toBeInTheDocument();
    expect(within(unknown).queryByText("$0.00")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /2026-10-16/ })).toHaveAttribute("href", row.cost?.source_url);
    expect(screen.getAllByText("-$0.05")).toHaveLength(2);
    expect(
      within(screen.getByRole("row", { name: /priced-model/ })).getByText(/Codex subscription/),
    ).toBeInTheDocument();
  });

  it("keeps reported zero costs visible and reports missing usage separately", () => {
    const zero = {
      ...row.cost,
      total_usd: 0,
      input_usd: 0,
      output_usd: 0,
      cache_read_usd: 0,
      cache_write_usd: 0,
      without_cache_usd: 0,
      cache_read_savings_usd: 0,
      cache_write_premium_usd: 0,
      net_savings_usd: 0,
    };
    show([
      { ...row, cost: zero },
      { ...row, model: "unreported", cost: null, priced_calls: 0, unpriced_calls: 2, usage_missing_calls: 2 },
    ]);
    const priced = screen.getByRole("row", { name: /priced-model/ });
    expect(within(priced).getAllByText("$0.00").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/2 calls without reported usage/).length).toBeGreaterThan(0);
  });
});
