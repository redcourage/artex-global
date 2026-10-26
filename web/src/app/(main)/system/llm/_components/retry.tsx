"use client";

// LLM 재시도 설정 공통 컴포넌트: 다섯 계층별 횟수 + 간격.
//
// 안쪽부터: 연결(SDK) → 빈 응답(SDK) → 동일 provider 안전 구간 → 순환 차단 → 의도 재실행.
// 앞 세 계층은 엔드포인트별이므로 각 모델 설정이 전역 기본값을 재정의할 수 있고 뒤 두 계층은 프로세스 전역이다.
//
// 모든 입력은 백엔드 db.RetryRule과 동일한 빈 값=미설정 규칙을 따른다.
//   횟수: 빈 값/0=내장 기본값 | -1=해당 계층 재시도 끄기 | >0=지정 횟수
//   간격: 빈 값/0=해당 계층 기본 지수 백오프 | >0=지정 고정 밀리초 간격

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

// 각 계층의 횟수 생략 시 기본값은 입력 플레이스홀더 전용이다. 나머지 문구(제목/조건/설명 등)는
// i18n(llmPage.retryLayer.<계층>.*)을 사용해 실행 언어로 표시한다.
export const RETRY_LAYERS = {
  connect: { defAttempts: 3 },
  empty: { defAttempts: 2 },
  stream: { defAttempts: 2 },
  breaker: { defAttempts: 3 },
  intent: { defAttempts: 2 },
} satisfies Record<string, { defAttempts: number }>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초를 읽기 쉬운 단위로 바꿔 입력 옆에 표시한다. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어형 숫자 입력: 빈 문자열 ↔ 0. 중간 입력("-", "1e")은 부모에 전달하지 않고 로컬에 유지한다. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 부모가 전체 값을 교체(정책 읽기/설정 전환)하면 따라간다. 직접 입력할 때는
  // value가 이미 로컬 텍스트를 해석한 값과 같으므로 이 경로로 오지 않는다.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 재시도 계층의 두 설정. idPrefix는 같은 페이지에 여러 번 표시할 때 label의 htmlFor를 구별한다. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true면 설정 서랍용 간결한 보기. 상세 설명 없이 해당 계층에 도달하는 오류만 표시한다. */
  compact?: boolean;
}) {
  const t = useTranslations("llmPage");
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{t(`retryLayer.${layer}.title`)}</Label>
          <span className="text-muted-foreground text-xs">{t(`retryLayer.${layer}.where`)}</span>
        </div>
        {/* 해당 계층에 도달하는 오류를 상태 코드까지 설명한다. 설정해도 효과가 없다면 그 오류가 다른 계층일 수 있다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">{t("retry.triggerLabel")}</span>:{" "}
          {t(`retryLayer.${layer}.trigger`)}
        </p>
        {!compact && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">{t("retry.skipsLabel")}</span>:{" "}
            {t(`retryLayer.${layer}.skips`)}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{t(`retryLayer.${layer}.desc`)}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {t(`retryLayer.${layer}.attemptsLabel`)}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={t("retry.defaultN", { n: meta.defAttempts })}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            {t("retry.intervalMs")}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder={t("retry.defaultBackoff")}
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">
            {human ? t("retry.fixed", { human }) : t(`retryLayer.${layer}.defInterval`)}
          </span>
        </div>
      </div>
      {!compact && (
        <p className="text-muted-foreground text-xs">
          {t("retry.emptyLine", { hint: t(`retryLayer.${layer}.offHint`) })}
        </p>
      )}
    </div>
  );
}

/** 모델 설정 서랍의 세 계층 재정의(엔드포인트에 종속된 계층). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  const t = useTranslations("llmPage");
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">{t("retry.override.label")}</Label>
        <p className="text-muted-foreground text-xs">{t("retry.override.hint")}</p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 재시도 및 백오프 탭: 다섯 계층 전역 기본값. */
export function RetryPolicyPanel() {
  const t = useTranslations("llmPage");
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(t("retry.toast.loadFailed", { msg: (e as Error).message }));
    } finally {
      setLoading(false);
    }
  }, [t]);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드가 범위를 벗어난 값을 보정해 반환하므로 반환값으로 갱신해 저장된 값을 그대로 표시한다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success(t("retry.toast.saved"));
    } catch (e) {
      toast.error(t("retry.toast.saveFailed", { msg: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> {t("retry.loading")}
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        {t.rich("retry.intro", {
          chain: (c) => <span className="text-foreground">{c}</span>,
          emph: (c) => <span className="text-foreground">{c}</span>,
        })}
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          {t("retry.save")}
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          {t("retry.resetAll")}
        </Button>
      </div>
    </div>
  );
}
