"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 새 버전이 시작될 때까지의 최대 대기 시간. 업데이트는 프로세스 시작 세 번(임시 저장 → 교체 → 새 버전)을 거치며
 * 각 단계는 수 초다. 느린 디스크와 Docker 컨테이너 재생성을 포함해 3분이면 충분하다. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const t = useTranslations("settingsPage");
  const richTags = {
    code: (chunks: React.ReactNode) => <span className="font-mono">{chunks}</span>,
  };
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress와 분리: 임시 저장 후 프로세스가 종료되어 SSE가 끊기므로 /api/health 폴링으로 전환한다.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet는 백엔드 캐시 우회 여부도 결정한다. 진입 시 자동 확인은 상단에서 이미 확인했으므로 캐시를 사용하며
  // 수동 업데이트 확인은 강제 조회해 방금 배포한 버전도 캐시 만료를 기다리지 않고 확인한다.
  const check = React.useCallback(
    (quiet = false) => {
      setChecking(true);
      api
        .checkUpdate(!quiet)
        .then((r) => {
          setInfo(r);
          if (!quiet) {
            if (r.error) toast.error(t("update.toast.checkFailed", { error: r.error }));
            else if (r.has_update) toast.success(t("update.toast.found", { latest: r.latest ?? "" }));
            else if (r.comparable) toast.success(t("update.toast.upToDate"));
          }
        })
        .catch((e) => {
          if (!quiet) toast.error(t("update.toast.checkFailed", { error: (e as Error).message }));
        })
        .finally(() => setChecking(false));
    },
    [t],
  );

  React.useEffect(() => {
    check(true);
  }, [check]);

  // 버전이 바뀔 때까지 /api/health를 폴링한다.
  //
  // 연결 복구가 아닌 버전 변경을 기준으로 삼아야 한다. 교체 중 이전 버전이 잠깐 다시 시작해
  // artex.new로 교체하고 즉시 종료하므로 연결 여부만 보면 성공으로 오판한다.
  const waitForNewVersion = React.useCallback(
    async (fromVersion: string) => {
      setRestarting(true);
      const deadline = Date.now() + RESTART_TIMEOUT_MS;
      while (Date.now() < deadline) {
        await sleep(2000);
        try {
          const r = await fetch("/api/health", { cache: "no-store" });
          if (r.ok) {
            const j = (await r.json()) as { version?: string };
            if (j.version && j.version !== fromVersion) {
              toast.success(t("update.toast.updated", { version: j.version }));
              await sleep(800);
              window.location.reload();
              return;
            }
          }
        } catch {
          // 재시작 중 연결 불가는 예상된 상황이므로 폴링을 계속한다.
        }
      }
      setRestarting(false);
      toast.error(t("update.toast.restartTimeout"));
    },
    [t],
  );

  // 업데이트 진행률 구독. Next의 /api 재작성은 버퍼링으로 이벤트를 막으므로 SSE에 사용하지 않는다.
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error(t("update.toast.updateFailed", { error: p.error || p.message }));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // 프로세스가 종료되면 SSE도 끊긴다. 이미 재시작 대기 중이면 정상이며
        // /api/health 폴링으로 계속 판정한다.
        es.close();
      };
      return es;
    },
    [waitForNewVersion, t],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${t("update.confirm.title", { latest: info.latest ?? "" })}\n\n` +
        t("update.confirm.restart") +
        (info.mode === "docker" ? `\n\n${t("update.confirm.docker")}` : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: t("update.toast.preparing") });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error(t("update.toast.startFailed", { error: (e as Error).message }));
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (!window.confirm(t("update.confirm.rollback"))) return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success(t("update.toast.rollbackSwitched"));
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error(t("update.toast.rollbackFailed", { error: (e as Error).message }));
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 다운로드 단계만 Content-Length로 실제 비율을 계산할 수 있다. 검증/압축 해제/재시작 대기는
  // 소요 시간을 알 수 없어 막대를 채우고 맥박 애니메이션으로 작업 중임을 표시한다.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 설정 페이지는 다중 열 배치이며 카드가 행 간격을 지정하고 열 사이에서 분할되지 않게 한다(page.tsx 주석 참조).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          {t("update.title")}
        </CardTitle>
        <CardDescription>{t("update.desc")}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">{t("update.currentVersion")}</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">
                {info.mode === "docker" ? t("update.modeDocker") : t("update.modeStandalone")}
              </Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">{t("update.latestVersion")}</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              {t("update.changelog")} <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {t("update.connectError", { error: info.error })}
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {t("update.noAsset", { latest: info.latest ?? "", os: info.os, arch: info.arch, asset: info.asset ?? "" })}
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            {t("update.willDownloadPrefix")}
            <span className="font-mono">{info.asset}</span>
            {info.size ? t("update.willDownloadSize", { size: humanSize(info.size) }) : ""}
            {t("update.willDownloadSuffix")}
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            {t("update.upToDate")}
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">{t.rich("update.dockerNote", richTags)}</p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">{restarting ? t("update.restarting") : progress?.message}</p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            {t("update.check")}
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? t("update.updateToVersion", { latest: info.latest ?? "" }) : t("update.updateNow")}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              {t("update.rollback")}
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">{t.rich("update.footer", richTags)}</p>
      </CardContent>
    </Card>
  );
}
