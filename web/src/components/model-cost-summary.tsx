"use client";

import { useTranslations } from "next-intl";

import { aggregateModelCosts, formatUSD, modelCostStatus } from "@/lib/model-cost";
import type { ModelTokenStat } from "@/lib/types";

const metrics = [
  ["withoutCache", "without_cache_usd"],
  ["readSavings", "cache_read_savings_usd"],
  ["writePremium", "cache_write_premium_usd"],
  ["netSavings", "net_savings_usd"],
] as const;

export function ModelCostSummary({ models }: { models: ModelTokenStat[] }) {
  const t = useTranslations("taskDetail.overviewTab.tokens.cost");
  const summary = aggregateModelCosts(models);
  const subtotalLabel = summary.status === "partial" ? t("partialSubtotal") : t("subtotal");

  return (
    <section className="space-y-3 border-t pt-3" aria-label={t("title")}>
      <div>
        <h3 className="font-medium text-sm">{t("title")}</h3>
        <p className="text-muted-foreground text-xs">{t("basis")}</p>
      </div>
      <dl className="grid grid-cols-2 gap-3 text-xs lg:grid-cols-5">
        <div>
          <dt className="text-muted-foreground">{subtotalLabel}</dt>
          <dd className="mt-1 font-semibold text-base tabular-nums">{formatUSD(summary.cost?.total_usd)}</dd>
          {summary.status === "unavailable" && <dd className="text-muted-foreground">{t("unavailable")}</dd>}
        </div>
        {metrics.map(([label, field]) => (
          <div key={field}>
            <dt className="text-muted-foreground">{t(label)}</dt>
            <dd className="mt-1 font-medium tabular-nums">{formatUSD(summary.cost?.[field])}</dd>
          </div>
        ))}
      </dl>
      <p className="text-muted-foreground text-xs">
        {summary.coverageUnknown
          ? t("coverageUnknown")
          : t("coverage", { priced: summary.pricedCalls, unpriced: summary.unpricedCalls })}
        {summary.usageMissingCalls > 0 && <> · {t("usageMissing", { count: summary.usageMissingCalls })}</>}
      </p>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th scope="col" className="py-2 pr-3 font-medium">
                {t("model")}
              </th>
              <th scope="col" className="py-2 pr-3 text-right font-medium">
                {t("estimate")}
              </th>
              {metrics.map(([label, field]) => (
                <th scope="col" key={field} className="whitespace-nowrap py-2 pr-3 text-right font-medium">
                  {t(label)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {models.map((model) => {
              const status = modelCostStatus(model);
              const cost = model.cost;
              return (
                <tr key={model.model} className="border-b align-top last:border-0">
                  <th scope="row" className="min-w-48 py-2 pr-3 text-left font-normal">
                    <div className="font-mono">{model.model}</div>
                    {cost?.source_url && (
                      <a
                        href={cost.source_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="text-muted-foreground underline underline-offset-2"
                      >
                        {t("pricingSource", { date: cost.price_version })}
                      </a>
                    )}
                    {cost && !cost.source_url && (
                      <p className="text-muted-foreground">
                        {cost.price_version === "mixed"
                          ? t("mixedPricing")
                          : t("pricingVersion", { date: cost.price_version })}
                      </p>
                    )}
                    {model.usage_sources?.includes("codex_subscription") && (
                      <p className="mt-1 text-muted-foreground">{t("subscription")}</p>
                    )}
                    {(model.usage_missing_calls ?? 0) > 0 && (
                      <p className="mt-1 text-muted-foreground">
                        {t("usageMissing", { count: model.usage_missing_calls ?? 0 })}
                      </p>
                    )}
                    {cost && (
                      <details className="mt-1 text-muted-foreground">
                        <summary className="cursor-pointer">{t("breakdown")}</summary>
                        <dl className="mt-1 space-y-1">
                          {(
                            [
                              ["input", "input_usd"],
                              ["cacheRead", "cache_read_usd"],
                              ["cacheWrite", "cache_write_usd"],
                              ["output", "output_usd"],
                            ] as const
                          ).map(([label, field]) => (
                            <div key={field} className="flex justify-between gap-3">
                              <dt>{t(label)}</dt>
                              <dd className="tabular-nums">{formatUSD(cost[field])}</dd>
                            </div>
                          ))}
                        </dl>
                      </details>
                    )}
                  </th>
                  <td className="py-2 pr-3 text-right tabular-nums">
                    <div>{formatUSD(cost?.total_usd)}</div>
                    {status !== "complete" && <div className="mt-1 text-muted-foreground">{t(status)}</div>}
                    {model.unpriced_calls != null && model.unpriced_calls > 0 && (
                      <div className="mt-1 text-muted-foreground">{t("unpriced", { count: model.unpriced_calls })}</div>
                    )}
                  </td>
                  {metrics.map(([, field]) => (
                    <td key={field} className="whitespace-nowrap py-2 pr-3 text-right tabular-nums">
                      {formatUSD(cost?.[field])}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <p className="text-muted-foreground text-xs">{t("scope")}</p>
    </section>
  );
}
