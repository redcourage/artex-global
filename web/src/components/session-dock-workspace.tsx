"use client";

import { useTranslations } from "next-intl";

import { type DockLabels, DockWorkspace, type DockWorkspaceProps } from "@/components/dock-workspace";

export type { DockGripProps } from "@/components/dock-workspace";

// Session-specific copy only; the workspace itself can host graphs, broadcasts,
// or any other stable panel ID without knowing about sessions or their streams.
export function SessionDockWorkspace(props: Omit<DockWorkspaceProps, "labels">) {
  const t = useTranslations("sessions.live.dock");
  const keys = [
    "hint",
    "arrange",
    "source",
    "target",
    "position",
    "apply",
    "grip",
    "resize",
    "moved",
    "restore",
    "left",
    "right",
    "top",
    "bottom",
    "center",
  ] as const;
  const labels = Object.fromEntries(keys.map((key) => [key, t(key)])) as DockLabels;
  return <DockWorkspace {...props} labels={labels} />;
}
