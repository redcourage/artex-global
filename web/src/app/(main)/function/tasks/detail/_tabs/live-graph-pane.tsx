"use client";

import * as React from "react";

import { GripVerticalIcon, MapIcon, NetworkIcon, XIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import type { DockGripProps } from "@/components/dock-workspace";
import { ExplorationGraph } from "@/components/exploration-graph";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { type GraphSnapshot as GraphData, shareGraphSnapshot } from "@/lib/graph-snapshot";

export const LIVE_GRAPH_ID = "exploration-graph-pane";
const Canvas = React.memo(
  function Canvas({ data, details }: { data: GraphData; details: boolean; resizing: boolean }) {
    return (
      <ExplorationGraph nodes={data.nodes} edges={data.edges} compact={!details} className="h-full rounded-none" />
    );
  },
  (previous, next) => next.resizing || (previous.data === next.data && previous.details === next.details),
);

export function LiveGraphPane({ taskId, grip, onClose }: { taskId: string; grip: DockGripProps; onClose: () => void }) {
  const t = useTranslations("sessions.live");
  const [data, setData] = React.useState<GraphData | null>(null);
  const [error, setError] = React.useState(false);
  const [retry, setRetry] = React.useState(0);
  const [details, setDetails] = React.useState(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry explicitly starts a new request and replaces the polling loop.
  React.useEffect(() => {
    let cancelled = false;
    let busy = false;
    const load = async () => {
      if (busy || document.hidden) return;
      busy = true;
      try {
        const response = await api.explorationGraph(taskId);
        if (cancelled) return;
        const next = { nodes: response.nodes ?? [], edges: response.edges ?? [] };
        setData((previous) => shareGraphSnapshot(previous, next));
        setError(false);
      } catch {
        if (!cancelled) setError(true);
      } finally {
        busy = false;
      }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    document.addEventListener("visibilitychange", load);
    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", load);
    };
  }, [taskId, retry]);
  return (
    <section
      data-live-graph
      className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-lg border bg-card"
      aria-label={t("graph")}
    >
      <header className="flex h-10 shrink-0 items-center gap-2 border-b px-2">
        <button
          type="button"
          onPointerDown={grip.onPointerDown}
          title={grip.title}
          aria-label={grip.title}
          className="cursor-grab touch-none rounded p-1 text-muted-foreground hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring active:cursor-grabbing"
        >
          <GripVerticalIcon className="size-4" />
        </button>
        <NetworkIcon className="size-3.5 text-muted-foreground" />
        <span className="mr-auto truncate text-sm font-medium">{t("graph")}</span>
        <Button
          size="icon-sm"
          variant={details ? "secondary" : "ghost"}
          className="size-7"
          aria-pressed={details}
          aria-label={t("graphDetails")}
          title={t("graphDetails")}
          onClick={() => setDetails((value) => !value)}
        >
          <MapIcon />
        </Button>
        <Button
          size="icon-sm"
          variant="ghost"
          className="size-7"
          onClick={onClose}
          aria-label={t("graphClose")}
          title={t("graphClose")}
        >
          <XIcon />
        </Button>
      </header>
      {error && (
        <div
          role="status"
          className="flex shrink-0 items-center gap-2 border-b px-3 py-1 text-xs text-muted-foreground"
        >
          <span>{t("graphError")}</span>
          <Button size="sm" variant="ghost" onClick={() => setRetry((value) => value + 1)}>
            {t("graphRetry")}
          </Button>
        </div>
      )}
      <div className="relative min-h-0 flex-1 overflow-hidden">
        <div data-dock-resize-content data-dock-resize-height className="absolute inset-0 h-full w-full">
          {data ? (
            <Canvas data={data} details={details} resizing={!!grip.resizing} />
          ) : (
            !error && (
              <div
                role="status"
                className="flex h-full items-center justify-center gap-2 text-sm text-muted-foreground"
              >
                <Spinner />
                {t("graphLoading")}
              </div>
            )
          )}
        </div>
      </div>
    </section>
  );
}
