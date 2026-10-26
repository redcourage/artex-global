"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { api } from "@/lib/api";

/**
 * 상단 새 버전 알림: 전체 페이지 로드 시 한 번 확인하고 업데이트가 있으면 버전 옆에 표시한다.
 * 클릭하면 시스템 설정의 버전 및 업데이트 카드로 이동한다.
 *
 * 백엔드가 GitHub 조회 결과를 30분 캐시하므로 마운트마다 확인해도 된다.
 * 비인증 GitHub API는 IP당 시간당 60회뿐이므로 캐시가 없으면 여러 탭에서
 * 할당량을 소진해 실제 업데이트가 필요할 때 조회할 수 없게 된다.
 *
 * 조회 실패는 조용히 처리한다. 사용자는 설정 페이지에서 업데이트 확인을 눌러 사유를 볼 수 있다.
 */
export function UpdateBadge() {
  const t = useTranslations("header");
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update에는 버전 비교 가능 여부가 포함되어 개발 빌드에서는 알림을 표시하지 않는다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 네트워크 없음/GitHub 속도 제한은 상단 바에 오류를 띄우지 않는다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={t("updateTitle", { latest })}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 맥박 점: 상단 요소가 많아 글자만으로 놓치기 쉬우므로 애니메이션으로 눈에 띄게 한다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">{t("updateBadge", { latest })}</span>
      <span className="sm:hidden">{t("updateBadgeShort")}</span>
    </Link>
  );
}
