"use client";

import { CheckIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText/inputType은 컨트롤 렌더링과 밀접한 이 파일의 값 보조 함수이므로 channel-fields에 두지 않는다.
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText는 임의 설정값을 입력에 사용할 문자열로 변환한다.
// config는 JSON에서 오므로 string/number/boolean/array/null일 수 있다.
// 여기서는 텍스트 입력 가능 여부만 처리하고 구체적인 직렬화는 buildConfig가 맡는다.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType은 필드 유형을 input의 type 속성으로 연결한다.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField는 필드 정의에 맞는 컨트롤을 표시한다.
//
// 마스킹 필드는 값 자체를 입력에 표시하지 않고 저장됨 안내만 표시한다.
// 따라서 입력에 글자가 있으면 사용자 입력이고 빈 입력이면 빈 값이라는 단일 규칙을 유지한다.
// "__masked__:…abc123"를 입력에 넣으면 사용자가 지워야 하는 플레이스홀더라고 오해해
// 자격 증명을 실수로 지울 가능성이 커진다.
//
// def.label / def.help / def.options[].label은 notifyPage 네임스페이스의 i18n key이며
// 이곳에서 t()로 해석한다(channel-fields.ts 참조).
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const t = useTranslations("notifyPage");
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // 백엔드 마스킹 값 형식은 "__masked__:…abc123"이며 끝부분은 원본을 식별할 수 있는 일부다.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={t(def.label)} />
        {t(def.label)}
        {def.help && <span className="text-muted-foreground">({t(def.help)})</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{t(def.label)}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {t(o.label)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // 필드 유형에 따라 컨트롤을 선택한다. 네 종류를 구분해야 하므로 중첩 삼항식 대신
  // if 체인을 사용해 가독성을 유지한다.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      {maskedTail ? t("masked.savedTail", { tail: maskedTail }) : t("masked.saved")}
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{t(def.help)}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{t(def.label)}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary는 필터를 한 줄로 요약해 카드를 펼치지 않고 발송 대상을 확인하게 한다.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const t = useTranslations("notifyPage");
  const parts: string[] = [];
  if (filter.min_severity) {
    const opt = SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity);
    parts.push(opt ? t(opt.label) : filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(t("summary.includeCount", { n: filter.vulnclass_include.length }));
  if (filter.vulnclass_exclude?.length) parts.push(t("summary.excludeCount", { n: filter.vulnclass_exclude.length }));
  if (filter.task_ids?.length) parts.push(t("summary.taskCount", { n: filter.task_ids.length }));
  if (filter.asset_ids?.length) parts.push(t("summary.assetCount", { n: filter.asset_ids.length }));
  if (filter.on_status_change) parts.push(t("summary.statusChange"));
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">{t("summary.all")}</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
