"use client";

import * as React from "react";

import { createPortal } from "react-dom";

import {
  type DockNode,
  type DockRect,
  type DockZone,
  dockDropTarget,
  dockGeometry,
  dockMinimum,
  dockMove,
  dockPreview,
  dockResizeWithin,
} from "@/lib/panel-dock";

export type DockLabels = Record<
  DockZone | "hint" | "arrange" | "source" | "target" | "position" | "apply" | "grip" | "resize" | "moved" | "restore",
  string
>;
export type DockPane = {
  id: string;
  label: string;
  minimumSize?: { width: number; height: number };
  render: (grip: DockGripProps) => React.ReactNode;
};
export type DockWorkspaceProps = {
  tree: DockNode;
  onChange: (tree: DockNode) => void;
  panes: DockPane[];
  labels: DockLabels;
  toolbarContainer?: HTMLElement | null;
  externalDrop?: { type: string; label: string; onDrop: (value: string, target: string, zone: DockZone) => void };
};
type DragPreview = {
  id: string;
  base: DockNode;
  preview: DockNode;
  drop: { target: string; zone: DockZone } | null;
};
export type DockGripProps = {
  onPointerDown: React.PointerEventHandler<HTMLButtonElement>;
  title: string;
  resizing?: boolean;
  expanded?: boolean;
  onToggleExpand?: () => void;
};
const position = (r: DockRect): React.CSSProperties => ({
  left: `${r.x * 100}%`,
  top: `${r.y * 100}%`,
  width: `${r.width * 100}%`,
  height: `${r.height * 100}%`,
});

// Read first, then write: text and graph canvases keep their raster/layout while
// their shells move. Restore only once on release, cancellation, or unmount.
function freezeContent(element: HTMLElement) {
  const items = Array.from(element.querySelectorAll<HTMLElement>("[data-dock-resize-content]"), (node) => {
    const rect = node.getBoundingClientRect();
    return {
      node,
      width: rect.width,
      height: node.hasAttribute("data-dock-resize-height") ? rect.height : 0,
      originalWidth: node.style.width,
      originalHeight: node.style.height,
    };
  });
  for (const item of items) {
    if (item.width > 0) item.node.style.width = `${item.width}px`;
    if (item.height > 0) item.node.style.height = `${item.height}px`;
  }
  return () => {
    for (const item of items) {
      item.node.style.width = item.originalWidth;
      if (item.height > 0) item.node.style.height = item.originalHeight;
    }
  };
}

// Keep the expanded content at its natural size while its shell closes around it.
// Only this panel's border/layout animates; transcript wrapping resumes once at rest.
function freezePanelContents(element: HTMLElement) {
  const section = element.querySelector(":scope > section");
  const items = Array.from(section?.children ?? [])
    .filter((node): node is HTMLElement => node instanceof HTMLElement)
    .map((node) => ({
      node,
      size: { width: node.offsetWidth, height: node.offsetHeight },
      width: node.style.width,
      height: node.style.height,
      flex: node.style.flex,
    }));
  for (const { node, size } of items) {
    node.style.width = `${size.width}px`;
    node.style.height = `${size.height}px`;
    node.style.flex = "none";
  }
  return () => {
    for (const { node, width, height, flex } of items) {
      node.style.width = width;
      node.style.height = height;
      node.style.flex = flex;
    }
  };
}

function splitterPosition(h: ReturnType<typeof dockGeometry>["handles"][number]): React.CSSProperties {
  const r = h.rect;
  return h.axis === "x"
    ? {
        left: `calc(${(r.x + r.width * h.ratio) * 100}% - 4px)`,
        top: `${r.y * 100}%`,
        width: 8,
        height: `${r.height * 100}%`,
        cursor: "col-resize",
      }
    : {
        left: `${r.x * 100}%`,
        top: `calc(${(r.y + r.height * h.ratio) * 100}% - 4px)`,
        width: `${r.width * 100}%`,
        height: 8,
        cursor: "row-resize",
      };
}

export function DockWorkspace({ tree, onChange, panes, labels, toolbarContainer, externalDrop }: DockWorkspaceProps) {
  const t = (key: keyof DockLabels) => labels[key];
  const canvas = React.useRef<HTMLDivElement>(null);
  const cleanup = React.useRef<(() => void) | null>(null);
  const animations = React.useRef(new Map<HTMLElement, Animation>());
  const animationRestores = React.useRef(new Map<HTMLElement, () => void>());
  const pendingCloseRestore = React.useRef<(() => void) | null>(null);
  function cancelAnimations() {
    for (const animation of animations.current.values()) animation.cancel();
    animations.current.clear();
    for (const restore of animationRestores.current.values()) restore();
    animationRestores.current.clear();
  }
  const beforeLayout = React.useRef<Map<HTMLElement, DOMRect> | null>(null);
  function captureLayout() {
    beforeLayout.current = new Map(
      Array.from(canvas.current?.querySelectorAll<HTMLElement>("[data-dock-pane]") ?? [], (node) => [
        node,
        node.getBoundingClientRect(),
      ]),
    );
  }
  const [expanded, setExpanded] = React.useState<string | null>(null);
  const [focusBounds, setFocusBounds] = React.useState<React.CSSProperties>({});
  const focusTransition = React.useRef<string | null>(null);
  const expandOrigin = React.useRef<HTMLElement | null>(null);
  const focusPane = expanded && panes.some((pane) => pane.id === expanded) ? expanded : null;
  function toggleExpand(id: string) {
    cleanup.current?.();
    captureLayout();
    focusTransition.current = id;
    if (focusPane === id) {
      const node = Array.from(canvas.current?.querySelectorAll<HTMLElement>("[data-dock-pane]") ?? []).find(
        (pane) => pane.dataset.dockPane === id,
      );
      if (node) pendingCloseRestore.current = freezePanelContents(node);
      setExpanded(null);
      expandOrigin.current?.focus({ preventScroll: true });
    } else {
      expandOrigin.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      const viewport = canvas.current?.parentElement;
      if (viewport)
        setFocusBounds({
          left: viewport.scrollLeft + viewport.clientWidth * 0.04,
          top: viewport.scrollTop + viewport.clientHeight * 0.04,
          width: viewport.clientWidth * 0.92,
          height: viewport.clientHeight * 0.92,
        });
      setExpanded(id);
    }
  }
  React.useLayoutEffect(() => {
    if (!focusPane) {
      if (expanded) setExpanded(null);
      return;
    }
    const viewport = canvas.current?.parentElement;
    if (!viewport) return;
    const measure = () =>
      setFocusBounds({
        left: viewport.scrollLeft + viewport.clientWidth * 0.04,
        top: viewport.scrollTop + viewport.clientHeight * 0.04,
        width: viewport.clientWidth * 0.92,
        height: viewport.clientHeight * 0.92,
      });
    measure();
    const observer = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    observer?.observe(viewport);
    viewport.addEventListener("scroll", measure);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      viewport.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
    };
  }, [focusPane, expanded]);
  React.useEffect(() => {
    if (!focusPane) return;
    const onKey = (event: KeyboardEvent) => {
      // A picker/dialog gets to consume Escape before the panel does.
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      toggleExpand(focusPane);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [focusPane]);
  const [drag, setDrag] = React.useState<DragPreview | null>(null);
  const [incoming, setIncoming] = React.useState<{ target: string; zone: DockZone } | null>(null);
  React.useEffect(() => {
    const clear = () => setIncoming(null);
    window.addEventListener("dragend", clear);
    window.addEventListener("drop", clear);
    return () => {
      window.removeEventListener("dragend", clear);
      window.removeEventListener("drop", clear);
    };
  }, []);
  const [resizing, setResizing] = React.useState<string | null>(null);
  const resizeBase = React.useRef<{ tree: string; panes: string } | null>(null);
  const latestTree = React.useRef(tree);
  const moving = drag !== null;
  const [source, setSource] = React.useState("");
  const [target, setTarget] = React.useState("");
  const [zone, setZone] = React.useState<DockZone>("right");
  const [announcement, setAnnouncement] = React.useState("");
  const geometry = dockGeometry(drag?.preview ?? tree);
  const originalGeometry = drag ? dockGeometry(drag.base) : geometry;
  const minimum = dockMinimum(tree, Object.fromEntries(panes.map((p) => [p.id, p.minimumSize])));
  const treeSignature = JSON.stringify(tree);
  const paneSignature = JSON.stringify(panes.map((p) => p.id).sort());
  React.useLayoutEffect(() => {
    latestTree.current = tree;
    const base = resizeBase.current;
    if (base && (base.tree !== treeSignature || base.panes !== paneSignature)) cleanup.current?.();
  }, [tree, treeSignature, paneSignature]);
  React.useEffect(() => {
    if (
      drag &&
      (JSON.stringify(drag.base) !== treeSignature ||
        JSON.stringify(Object.keys(dockGeometry(drag.base).panes).sort()) !== paneSignature)
    ) {
      cleanup.current?.();
    }
  }, [drag, treeSignature, paneSignature]);
  React.useLayoutEffect(() => {
    const before = beforeLayout.current;
    beforeLayout.current = null;
    if (!before) return;
    // Dock/open motions use FLIP. Closing resizes just the shell around frozen content.
    // Retarget from the current visual rectangle if a prior animation is active.
    cancelAnimations();
    const after = new Map(Array.from(before.keys(), (node) => [node, node.getBoundingClientRect()]));
    const focusId = focusTransition.current;
    focusTransition.current = null;
    let restoreClosing = pendingCloseRestore.current;
    pendingCloseRestore.current = null;
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      restoreClosing?.();
      return;
    }
    for (const [node, old] of before) {
      const rect = after.get(node)!;
      if (node.dataset.dockDragging || !node.animate || !old.width || !old.height || !rect.width || !rect.height)
        continue;
      const x = old.left - rect.left,
        y = old.top - rect.top,
        sx = old.width / rect.width,
        sy = old.height / rect.height;
      if (Math.abs(x) + Math.abs(y) + Math.abs(sx - 1) + Math.abs(sy - 1) < 0.01) continue;
      const focusMotion = focusId === node.dataset.dockPane;
      const layer = focusMotion ? { zIndex: 50 } : {};
      const closing = focusMotion && !focusPane;
      const frames: Keyframe[] = closing
        ? [
            {
              transform: `translate3d(${x}px, ${y}px, 0)`,
              width: `${old.width}px`,
              height: `${old.height}px`,
              ...layer,
            },
            { transform: "translate3d(0, 0, 0)", width: `${rect.width}px`, height: `${rect.height}px`, ...layer },
          ]
        : [
            { transform: `translate3d(${x}px, ${y}px, 0) scale(${sx}, ${sy})`, ...layer },
            { transform: "translate3d(0, 0, 0) scale(1, 1)", ...layer },
          ];
      const animation = node.animate(frames, {
        duration: focusMotion ? 400 : 210,
        easing: focusMotion ? "cubic-bezier(.4,0,.2,1)" : "cubic-bezier(.16,1.14,.3,1)",
      });
      animations.current.set(node, animation);
      if (closing && restoreClosing) {
        animationRestores.current.set(node, restoreClosing);
        restoreClosing = null;
      }
      const finish = () => {
        if (animations.current.get(node) !== animation) return;
        animations.current.delete(node);
        animationRestores.current.get(node)?.();
        animationRestores.current.delete(node);
      };
      animation.onfinish = finish;
      animation.oncancel = finish;
    }
    restoreClosing?.();
  });
  React.useEffect(
    () => () => {
      cleanup.current?.();
      cancelAnimations();
      pendingCloseRestore.current?.();
      pendingCloseRestore.current = null;
    },
    [],
  );

  function listen(e: React.PointerEvent, move: (e: PointerEvent) => void, finish: (cancel: boolean) => void) {
    cleanup.current?.();
    const id = e.pointerId;
    const owner = e.currentTarget;
    owner.setPointerCapture?.(id);
    const onMove = (event: PointerEvent) => {
      if (event.pointerId === id) move(event);
    };
    const stop = (cancel: boolean) => {
      dispose();
      finish(cancel);
    };
    const up = (event: PointerEvent) => {
      if (event.pointerId === id) {
        move(event);
        stop(false);
      }
    };
    const cancel = () => stop(true);
    const pointerCancel = (event: PointerEvent) => {
      if (event.pointerId === id) cancel();
    };
    const key = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    };
    const dispose = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", up);
      window.removeEventListener("pointercancel", pointerCancel);
      window.removeEventListener("resize", cancel);
      owner.removeEventListener("lostpointercapture", cancel);
      window.removeEventListener("blur", cancel);
      window.removeEventListener("keydown", key);
      if (owner.hasPointerCapture?.(id)) owner.releasePointerCapture(id);
      cleanup.current = null;
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", up);
    window.addEventListener("pointercancel", pointerCancel);
    window.addEventListener("resize", cancel);
    owner.addEventListener("lostpointercapture", cancel);
    window.addEventListener("blur", cancel);
    window.addEventListener("keydown", key);
    cleanup.current = cancel;
  }

  function startDrag(e: React.PointerEvent<HTMLButtonElement>, id: string) {
    if (e.button !== 0 || !canvas.current || focusPane) return;
    e.preventDefault();
    e.currentTarget.focus();
    cleanup.current?.();
    const element = canvas.current;
    const sourceNode = Array.from(element.querySelectorAll<HTMLElement>("[data-dock-pane]")).find(
      (node) => node.dataset.dockPane === id,
    );
    if (!sourceNode) return;
    const box = element.getBoundingClientRect();
    if (!box.width || !box.height) return;
    const scroll = element.parentElement;
    const scrollX = scroll?.scrollLeft ?? 0,
      scrollY = scroll?.scrollTop ?? 0;
    resizeBase.current = { tree: treeSignature, panes: paneSignature };
    const baseGeometry = dockGeometry(tree);
    const startX = e.clientX,
      startY = e.clientY;
    let active = false,
      frame = 0;
    let pointer = { x: startX, y: startY };
    let drop: { target: string; zone: DockZone } | null = null;
    let preview = tree;
    let restoreContent: (() => void) | undefined;
    const scrollDelta = () => ({ x: (scroll?.scrollLeft ?? 0) - scrollX, y: (scroll?.scrollTop ?? 0) - scrollY });
    const targetAt = (delta = scrollDelta()) =>
      dockDropTarget(
        baseGeometry.panes,
        id,
        (pointer.x - box.left + delta.x) / box.width,
        (pointer.y - box.top + delta.y) / box.height,
      );
    const paint = () => {
      frame = 0;
      const delta = scrollDelta();
      const next = targetAt(delta);
      const changed = !active || next?.target !== drop?.target || next?.zone !== drop?.zone;
      // All measurements precede style writes. No layout reads on same-zone moves.
      if (changed) captureLayout();
      if (!active) restoreContent = freezeContent(element);
      active = true;
      sourceNode.style.setProperty("--dock-drag-x", `${pointer.x - startX + delta.x}px`);
      sourceNode.style.setProperty("--dock-drag-y", `${pointer.y - startY + delta.y}px`);
      if (changed) {
        drop = next;
        preview = drop ? dockMove(tree, id, drop.target, drop.zone) : tree;
        setDrag({ id, base: tree, preview, drop });
      }
    };
    listen(
      e,
      (event) => {
        pointer = { x: event.clientX, y: event.clientY };
        if (!active && Math.hypot(pointer.x - startX, pointer.y - startY) < 6) return;
        if (!frame) frame = requestAnimationFrame(paint);
      },
      (cancel) => {
        cancelAnimationFrame(frame);
        resizeBase.current = null;
        // A release between display frames must still use its exact coordinates.
        const travelled = Math.hypot(pointer.x - startX, pointer.y - startY) >= 6;
        drop = !cancel && (active || travelled) ? targetAt() : null;
        if (active) captureLayout();
        restoreContent?.();
        sourceNode.style.removeProperty("--dock-drag-x");
        sourceNode.style.removeProperty("--dock-drag-y");
        setDrag(null);
        if (drop) {
          onChange(dockMove(tree, id, drop.target, drop.zone));
          setAnnouncement(t("moved"));
        }
      },
    );
  }

  function resizeWithin(ratio: number, id: string, box = canvas.current?.getBoundingClientRect()) {
    return dockResizeWithin(
      tree,
      id,
      ratio,
      box ?? { width: 0, height: 0 },
      Object.fromEntries(panes.map((pane) => [pane.id, pane.minimumSize])),
    );
  }

  function startResize(
    e: React.PointerEvent<HTMLDivElement>,
    handle: ReturnType<typeof dockGeometry>["handles"][number],
  ) {
    if (e.button !== 0 || !canvas.current || focusPane) return;
    e.preventDefault();
    e.currentTarget.focus();
    cleanup.current?.();
    const element = canvas.current;
    const box = element.getBoundingClientRect();
    if (!box.width || !box.height) return;
    const base = tree;
    const restoreContent = freezeContent(element);
    const paneNodes = Array.from(element.querySelectorAll<HTMLElement>("[data-dock-pane]"));
    const splitNodes = Array.from(element.querySelectorAll<HTMLElement>("[data-dock-split]"));
    const sizes = Object.fromEntries(panes.map((pane) => [pane.id, pane.minimumSize]));
    let pendingRatio = handle.ratio;
    const calculate = () => dockResizeWithin(tree, handle.id, pendingRatio, box, sizes);
    let preview = base;
    let frame = 0;
    const paint = (layout: DockNode) => {
      const geometry = dockGeometry(layout);
      // Geometry-only DOM writes: no React state updates or transcript rendering per pointer event.
      for (const node of paneNodes) {
        const rect = geometry.panes[node.dataset.dockPane!];
        if (rect) Object.assign(node.style, position(rect));
      }
      const handles = new Map(geometry.handles.map((item) => [item.id, item]));
      for (const node of splitNodes) {
        const split = handles.get(node.dataset.dockSplit!);
        if (split) {
          const style = splitterPosition(split);
          for (const [key, value] of Object.entries(style)) {
            node.style.setProperty(key, typeof value === "number" ? `${value}px` : String(value));
          }
          node.setAttribute("aria-valuenow", String(Math.round(split.ratio * 100)));
        }
      }
    };
    resizeBase.current = { tree: treeSignature, panes: paneSignature };
    setResizing(handle.id);
    listen(
      e,
      (event) => {
        const r = handle.rect;
        pendingRatio =
          handle.axis === "x"
            ? ((event.clientX - box.left) / box.width - r.x) / r.width
            : ((event.clientY - box.top) / box.height - r.y) / r.height;
        // A burst of pointer events produces at most one set of writes per display frame.
        if (!frame)
          frame = requestAnimationFrame(() => {
            frame = 0;
            preview = calculate();
            paint(preview);
          });
      },
      (cancel) => {
        cancelAnimationFrame(frame);
        resizeBase.current = null;
        // Restore normal text layout exactly once, after the last geometry write.
        if (!cancel) preview = calculate();
        paint(cancel ? latestTree.current : preview);
        restoreContent();
        setResizing(null);
        if (!cancel && JSON.stringify(preview) !== JSON.stringify(base)) onChange(preview);
      },
    );
  }

  const validSource = panes.some((p) => p.id === source) ? source : (panes[0]?.id ?? "");
  const validTarget = panes.some((p) => p.id === target && p.id !== validSource)
    ? target
    : (panes.find((p) => p.id !== validSource)?.id ?? "");
  const toolbar = (
    <div className="relative flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
      {!toolbarContainer && <span className="min-w-0 flex-1 truncate">{t("hint")}</span>}
      <details className="relative z-20">
        <summary
          title={t("hint")}
          className="cursor-pointer rounded px-2 py-1 focus-visible:outline-2 focus-visible:outline-ring"
        >
          {t("arrange")}
        </summary>
        <div className="absolute right-0 top-full flex w-72 flex-col gap-2 rounded-md border bg-popover p-3 text-popover-foreground shadow-md">
          <label>
            {t("source")}
            <select
              className="mt-1 w-full rounded border bg-background p-2"
              value={validSource}
              onChange={(e) => setSource(e.target.value)}
            >
              {panes.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.label}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t("target")}
            <select
              className="mt-1 w-full rounded border bg-background p-2"
              value={validTarget}
              onChange={(e) => setTarget(e.target.value)}
            >
              {panes
                .filter((p) => p.id !== validSource)
                .map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.label}
                  </option>
                ))}
            </select>
          </label>
          <label>
            {t("position")}
            <select
              className="mt-1 w-full rounded border bg-background p-2"
              value={zone}
              onChange={(e) => setZone(e.target.value as DockZone)}
            >
              {(["left", "right", "top", "bottom", "center"] as const).map((z) => (
                <option key={z} value={z}>
                  {t(z)}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className="rounded bg-primary p-2 text-primary-foreground disabled:opacity-50"
            disabled={!validTarget}
            onClick={() => {
              onChange(dockMove(tree, validSource, validTarget, zone));
              setAnnouncement(t("moved"));
            }}
          >
            {t("apply")}
          </button>
        </div>
      </details>
    </div>
  );
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-1">
      {toolbarContainer ? createPortal(toolbar, toolbarContainer) : toolbar}
      <div className="min-h-0 min-w-0 flex-1" style={{ overflow: focusPane ? "hidden" : "auto" }}>
        {/* biome-ignore lint/a11y/noStaticElementInteractions: Drop surface; session-list buttons and Arrange controls provide the keyboard equivalents. */}
        <div
          ref={canvas}
          data-dock-workspace
          data-dock-interacting={moving || !!resizing || undefined}
          onDragOver={(e) => {
            if (focusPane || !externalDrop || !e.dataTransfer.types.includes(externalDrop.type)) return;
            const box = e.currentTarget.getBoundingClientRect();
            const next = dockDropTarget(
              dockGeometry(tree).panes,
              "",
              (e.clientX - box.left) / box.width,
              (e.clientY - box.top) / box.height,
            );
            if (!next) return;
            e.preventDefault();
            e.dataTransfer.dropEffect = "copy";
            if (next.zone === "center") next.zone = "right";
            setIncoming((old) => (old?.target === next.target && old.zone === next.zone ? old : next));
          }}
          onDragLeave={(e) => {
            if (!(e.relatedTarget instanceof Node) || !e.currentTarget.contains(e.relatedTarget)) setIncoming(null);
          }}
          onDrop={(e) => {
            if (focusPane || !externalDrop || !e.dataTransfer.types.includes(externalDrop.type)) return;
            e.preventDefault();
            const box = e.currentTarget.getBoundingClientRect();
            const next = dockDropTarget(
              dockGeometry(tree).panes,
              "",
              (e.clientX - box.left) / box.width,
              (e.clientY - box.top) / box.height,
            );
            setIncoming(null);
            if (next)
              externalDrop.onDrop(
                e.dataTransfer.getData(externalDrop.type),
                next.target,
                next.zone === "center" ? "right" : next.zone,
              );
          }}
          className="relative h-full w-full"
          style={{
            minWidth: minimum.width,
            minHeight: minimum.height,
            userSelect: moving || resizing ? "none" : undefined,
          }}
        >
          <button
            type="button"
            data-dock-focus-backdrop
            aria-label={t("restore")}
            aria-hidden={!focusPane}
            tabIndex={-1}
            onClick={() => {
              if (focusPane) toggleExpand(focusPane);
            }}
            className="absolute z-40 cursor-default bg-black/20 backdrop-blur-[3px] transition-[opacity,visibility] duration-[400ms] motion-reduce:transition-none"
            style={{
              left: Number(focusBounds.left ?? 0) - (Number(focusBounds.width ?? 0) * 0.04) / 0.92,
              top: Number(focusBounds.top ?? 0) - (Number(focusBounds.height ?? 0) * 0.04) / 0.92,
              width: Number(focusBounds.width ?? 0) / 0.92,
              height: Number(focusBounds.height ?? 0) / 0.92,
              transitionTimingFunction: "cubic-bezier(.4,0,.2,1)",
              opacity: focusPane ? 1 : 0,
              visibility: focusPane ? "visible" : "hidden",
              pointerEvents: focusPane ? "auto" : "none",
            }}
          />
          {panes.map(
            (p) =>
              geometry.panes[p.id] && (
                <div
                  key={p.id}
                  data-dock-pane={p.id}
                  data-dock-expanded={focusPane === p.id || undefined}
                  inert={!!focusPane && focusPane !== p.id}
                  role={focusPane === p.id ? "region" : undefined}
                  aria-label={focusPane === p.id ? p.label : undefined}
                  data-dock-dragging={drag?.id === p.id || undefined}
                  className="absolute p-1 [&>section]:h-full"
                  style={{
                    ...position(drag?.id === p.id ? originalGeometry.panes[p.id] : geometry.panes[p.id]),
                    ...(focusPane === p.id ? focusBounds : {}),
                    contain: "layout paint",
                    transform:
                      drag?.id === p.id
                        ? "translate3d(var(--dock-drag-x), var(--dock-drag-y), 0) scale(0.98)"
                        : "translate3d(0, 0, 0)",
                    transformOrigin: "top left",
                    willChange: moving ? "transform" : undefined,
                    zIndex: focusPane === p.id ? 50 : drag?.id === p.id ? 30 : undefined,
                    pointerEvents: moving ? "none" : undefined,
                    opacity: drag?.id === p.id ? 0.88 : undefined,
                    boxShadow: drag?.id === p.id ? "0 12px 20px rgb(0 0 0 / 0.18)" : undefined,
                  }}
                >
                  {p.render({
                    onPointerDown: (e) => startDrag(e, p.id),
                    expanded: focusPane === p.id,
                    onToggleExpand: () => toggleExpand(p.id),
                    title: t("grip"),
                    resizing: !!resizing || moving,
                  })}
                </div>
              ),
          )}
          {!moving &&
            !focusPane &&
            geometry.handles.map((h) => {
              return (
                // biome-ignore lint/a11y/useSemanticElements: An interactive window splitter uses the separator widget role, not a document rule.
                <div
                  key={h.id}
                  data-dock-split={h.id}
                  data-resizing={resizing === h.id || undefined}
                  title={t("resize")}
                  role="separator"
                  tabIndex={0}
                  aria-label={t("resize")}
                  aria-orientation={h.axis === "x" ? "vertical" : "horizontal"}
                  aria-valuemin={20}
                  aria-valuemax={80}
                  aria-valuenow={Math.round(h.ratio * 100)}
                  className="absolute z-10 touch-none rounded hover:bg-ring/40 data-[resizing=true]:bg-ring/50 focus-visible:bg-ring/40 focus-visible:outline-2 focus-visible:outline-ring"
                  style={splitterPosition(h)}
                  onDoubleClick={() => onChange(resizeWithin(0.5, h.id))}
                  onKeyDown={(e) => {
                    const decrease = h.axis === "x" ? "ArrowLeft" : "ArrowUp",
                      increase = h.axis === "x" ? "ArrowRight" : "ArrowDown";
                    if (![decrease, increase, "Home", "End"].includes(e.key)) return;
                    e.preventDefault();
                    onChange(
                      resizeWithin(
                        e.key === "Home" ? 0.2 : e.key === "End" ? 0.8 : h.ratio + (e.key === decrease ? -0.05 : 0.05),
                        h.id,
                      ),
                    );
                  }}
                  onPointerDown={(e) => startResize(e, h)}
                />
              );
            })}
          {incoming && geometry.panes[incoming.target] && (
            <div
              data-dock-incoming
              className="pointer-events-none absolute z-40 p-1 transition-all duration-150 motion-reduce:transition-none"
              style={position(dockPreview(geometry.panes[incoming.target], incoming.zone))}
            >
              <div className="flex h-full items-center justify-center rounded-lg border-2 border-dashed border-ring bg-background/85 text-sm shadow-lg">
                {externalDrop?.label} · {t(incoming.zone)}
              </div>
            </div>
          )}
          {drag?.drop && geometry.panes[drag.id] && (
            <div
              data-dock-placeholder
              className="pointer-events-none absolute z-20 p-1 transition-all duration-200 motion-reduce:transition-none"
              style={position(geometry.panes[drag.id])}
            >
              <div className="flex h-full items-center justify-center rounded-lg border-2 border-dashed border-ring bg-primary/10">
                <span className="rounded bg-popover px-3 py-2 text-sm font-medium text-popover-foreground shadow-sm">
                  {t(drag.drop.zone)}
                </span>
              </div>
            </div>
          )}
        </div>
      </div>
      <span className="sr-only" aria-live="polite">
        {announcement}
      </span>
    </div>
  );
}
