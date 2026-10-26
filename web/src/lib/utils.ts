import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 서랍/대화 상자(Sheet/Dialog)의 onInteractOutside 닫기 판정 보조.
//
// 배경: 서랍 내 Radix 팝업(Select 드롭다운, DropdownMenu, Popover 등)은 서랍 밖으로
// portal된다. 팝업이 열린 상태에서 외부/배경 클릭으로 닫으려 하면 같은 pointerdown을 Select와 Sheet의
// DismissableLayer가 함께 처리한다. Select가 먼저 닫히며 discrete 이벤트라 React가 동기 flush하므로
// Sheet 처리기가 실행될 때 팝업 data-state는 이미 closed다. 따라서 처리 시점의 열린 상태를
// 검사하는 방식은 신뢰할 수 없다(실측 확인).
//
// Radix의 pointerdown 수신은 버블 단계이므로 그보다 앞선 capture 단계에서
// 팝업이 열려 있는지 기록한 뒤 onInteractOutside가 기록값을 읽어 닫기 허용 여부를 결정한다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix 버블 단계 pointerdown 처리기보다 먼저 기록한다.
  );
}

// radixOverlayWasOpenAtPointerDown은 최근 pointerdown 시 Radix 팝업이 열려 있었는지 반환한다.
// 서랍/대화 상자는 이를 통해 팝업이 열린 상태의 배경 클릭에서 팝업만 닫고 자신은 유지한다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText는 클립보드에 텍스트를 쓰고 성공 여부를 반환한다.
// navigator.clipboard는 보안 문맥(HTTPS/localhost)에서만 사용 가능하며 IP + HTTP에서는
// undefined이므로 execCommand("copy")로 대체한다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 대체 경로를 계속 진행한다.
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
