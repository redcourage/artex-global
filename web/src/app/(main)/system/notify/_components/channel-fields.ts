// 채널 필드 표와 설정값 해석 도구.
//
// 이 파일은 화면이 아닌 데이터이므로 페이지와 분리했다. 채널별 필드와
// 컨트롤 유형, 양식 텍스트와 설정값(JSON)의 양방향 변환을 정의한다.
// 새 채널을 추가할 때 이 파일만 수정하고 페이지는 유지할 수 있다.
//
// 순수 데이터 모듈(client component 아님)이므로 useTranslations를 호출할 수 없다.
// 문구를 하드코딩하는 대신 i18n key(notifyPage 네임스페이스)를 저장하고 렌더링하는
// 컴포넌트(page.tsx/channel-form.tsx)가 t()로 해석한다. key는 ASCII이며 상류 중국어 원문은
// messages/zh.json에 보존한다. 채널 유형 표시 이름은 notifyPage.kind.<kind>를 사용한다.

// 채널별 설정 필드 정의.
//
// 백엔드 schema를 받는 대신 프런트엔드 필드 표를 유지한다. 백엔드는
// Validate(필수/형식)를 맡고 UI는 배치와 컨트롤 유형을 관리하므로 관심사가 다르다.
// 유일한 연결점은 secret_keys다. 비밀번호 입력으로 표시할 필드는 백엔드가 제공한다.
// 어떤 값이 자격 증명인지는 채널 구현만 알 수 있다(WeCom은 Webhook 전체가 자격 증명이고
// DingTalk는 그중 secret만 해당). 새 채널이 표에서 빠지면 양식이 비어 보이며
// 조용히 실패하지 않는다(아래 hasFields가 안내).
//
// label/help/options[].label은 notifyPage 네임스페이스의 i18n key다.
// placeholder는 리터럴(URL/예시)로 유지하며 번역하지 않는다.
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  /** notifyPage 네임스페이스의 i18n key. */
  label: string;
  kind: FieldKind;
  placeholder?: string;
  /** notifyPage 네임스페이스의 i18n key. */
  help?: string;
  /** options[].label도 i18n key. */
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "field.dingtalk.webhook.label",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "field.dingtalk.secret.label",
      kind: "password",
      help: "field.dingtalk.secret.help",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "field.feishu.webhook.label",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "field.feishu.secret.label", kind: "password", help: "field.feishu.secret.help" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "field.wecom.webhook.label",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    {
      key: "url",
      label: "field.webhook.url.label",
      kind: "text",
      placeholder: "https://your-endpoint.example.com/hook",
    },
    {
      key: "method",
      label: "field.webhook.method.label",
      kind: "select",
      options: [
        { value: "POST", label: "field.webhook.method.opt.POST" },
        { value: "PUT", label: "field.webhook.method.opt.PUT" },
        { value: "PATCH", label: "field.webhook.method.opt.PATCH" },
        { value: "GET", label: "field.webhook.method.opt.GET" },
      ],
    },
    { key: "headers", label: "field.webhook.headers.label", kind: "kv", help: "field.webhook.headers.help" },
    {
      key: "body_template",
      label: "field.webhook.body_template.label",
      kind: "textarea",
      help: "field.webhook.body_template.help",
    },
  ],
  telegram: [
    { key: "bot_token", label: "field.telegram.bot_token.label", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "field.telegram.chat_id.label", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "field.telegram.base_url.label",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "field.telegram.base_url.help",
    },
  ],
  email: [
    { key: "host", label: "field.email.host.label", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "field.email.port.label",
      kind: "number",
      placeholder: "587",
      help: "field.email.port.help",
    },
    { key: "username", label: "field.email.username.label", kind: "text" },
    { key: "password", label: "field.email.password.label", kind: "password" },
    { key: "from", label: "field.email.from.label", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "field.email.to.label", kind: "list", help: "field.email.to.help" },
    { key: "tls", label: "field.email.tls.label", kind: "switch", help: "field.email.tls.help" },
  ],
};

// value는 필터용 min_severity 값, label은 notifyPage.severityOpt.*의 i18n key다.
export const SEVERITY_OPTIONS = [
  { value: "", label: "severityOpt.all" },
  { value: "low", label: "severityOpt.low" },
  { value: "medium", label: "severityOpt.medium" },
  { value: "high", label: "severityOpt.high" },
  { value: "critical", label: "severityOpt.critical" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV는 줄마다 KEY=VALUE인 텍스트 영역을 해석한다.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs는 쉼표/공백으로 구분한 id 목록을 해석한다. 전각 쉼표 「，」도 구분자로 유지한다.
// 사용자가 다른 곳에서 전각 쉼표가 포함된 목록을 붙여넣을 수 있으므로 이는 표시 문구가 아닌 입력 해석 로직이다.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords는 줄/쉼표로 구분한 키워드를 해석한다(취약점 유형에 공백이 있을 수 있어 줄/쉼표로 나눔).
// 마찬가지로 전각 쉼표 「，」를 구분자로 유지한다(표시가 아닌 입력 해석).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
