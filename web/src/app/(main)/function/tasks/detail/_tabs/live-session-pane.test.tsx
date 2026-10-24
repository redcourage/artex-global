import * as React from "react";

import "@testing-library/jest-dom/vitest";

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Activity, Session } from "@/lib/types";

import messages from "../../../../../../../messages/en.json";
import { type LiveScrollPosition, LiveSessionPane } from "./live-session-pane";

vi.mock("@/components/transcript", () => ({
  Transcript: ({ activity }: { activity: Activity[] }) => (
    <div data-testid="transcript">{activity.map((item) => item.summary).join("\n")}</div>
  ),
}));
const session: Session = {
  id: "14",
  intent_id: "14",
  role: "worker",
  title: "Boundary review",
  status: "running",
  live: true,
  last_activity: "",
};
const memory = new Map<string, LiveScrollPosition>();
let calls: number[] = [];
let observer: ResizeObserverCallback | undefined;
const callbacks = {
  onActivate: vi.fn(),
  onSelect: vi.fn(),
  onPin: vi.fn(),
  onOpen: vi.fn(),
  onRetry: vi.fn(),
  onLoadEarlier: vi.fn(),
  onFollowing: vi.fn(),
};
function view(items: Activity[] = [], overrides = {}) {
  return (
    <NextIntlClientProvider locale="en" messages={messages}>
      <LiveSessionPane
        taskId="task1"
        paneId="pane-3"
        number={3}
        session={session}
        title="Boundary review"
        history={{ items, loaded: true, loading: false, loadingMore: false, hasMore: false }}
        choices={[{ session, title: "Boundary review" }]}
        active
        pinned={false}
        connected
        pendingApproval={false}
        memory={memory}
        {...callbacks}
        {...overrides}
      />
    </NextIntlClientProvider>
  );
}
beforeEach(() => {
  memory.clear();
  calls = [];
  vi.clearAllMocks();
  vi.useFakeTimers();
  vi.stubGlobal("matchMedia", () => ({ matches: true }));
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(callback: ResizeObserverCallback) {
        observer = callback;
      }
      observe() {
        /* Invoked manually by the test. */
      }
      disconnect() {
        /* No native observer in jsdom. */
      }
    },
  );
  Object.defineProperty(HTMLElement.prototype, "scrollTo", {
    configurable: true,
    value(options: ScrollToOptions) {
      calls.push(options.top ?? 0);
      this.scrollTop = options.top ?? 0;
    },
  });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});
describe("independent live transcript navigation", () => {
  it("expands from the title or button, leaving header controls independent", () => {
    const expand = vi.fn();
    const r = render(
      view([], { dockGrip: { onPointerDown: vi.fn(), title: "Move", onToggleExpand: expand, expanded: false } }),
    );
    fireEvent.doubleClick(document.querySelector("[data-live-pane-header] > span")!);
    expect(expand).toHaveBeenCalledTimes(1);
    fireEvent.doubleClick(screen.getByRole("button", { name: /Choose a session/ }));
    fireEvent.doubleClick(screen.getByRole("button", { name: "Move" }));
    expect(expand).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Expand pane" }));
    expect(expand).toHaveBeenCalledTimes(2);
    r.rerender(
      view([], { dockGrip: { onPointerDown: vi.fn(), title: "Move", onToggleExpand: expand, expanded: true } }),
    );
    expect(screen.getByRole("button", { name: "Restore pane (Esc)" })).toHaveAttribute("aria-expanded", "true");
  });
  it("stops following when the reader scrolls up; SSE and delayed content do not move it until resumed", () => {
    const r = render(view());
    const viewport = screen.getByTestId("transcript").parentElement?.parentElement as HTMLDivElement;
    Object.defineProperties(viewport, {
      scrollHeight: { configurable: true, value: 1200 },
      clientHeight: { configurable: true, value: 300 },
    });
    act(() => vi.runOnlyPendingTimers());
    viewport.scrollTop = 200;
    fireEvent.wheel(viewport, { deltaY: -100 });
    fireEvent.scroll(viewport);
    calls = [];
    r.rerender(view([{ seq: 1, kind: "text", worker: "worker", ts: "", summary: "New evidence", intent_id: "14" }]));
    act(() => {
      observer?.([], {} as ResizeObserver);
      vi.runOnlyPendingTimers();
    });
    expect(calls).toEqual([]);
    expect(screen.getByTestId("transcript")).toHaveTextContent("New evidence");
    expect(viewport.scrollTop).toBe(200);
    fireEvent.click(screen.getByRole("button", { name: "Follow latest" }));
    expect(calls.at(-1)).toBe(1200);
    expect(callbacks.onFollowing).toHaveBeenLastCalledWith(true);
  });
  it("restores the same reading position after the pane is hidden and shown", () => {
    memory.set("pane-3:14", { top: 320, following: false });
    const r = render(view());
    const viewport = screen.getByTestId("transcript").parentElement?.parentElement as HTMLDivElement;
    expect(viewport.scrollTop).toBe(320);
    viewport.scrollTop = 410;
    r.unmount();
    expect(memory.get("pane-3:14")).toEqual({ top: 410, following: false });
    render(view());
    expect(screen.getByTestId("transcript").parentElement?.parentElement?.scrollTop).toBe(410);
  });
  it.each([
    true,
    false,
  ])("buffers transcript rendering and suspends automatic scrolling during resize (following=%s)", (following) => {
    if (!following) memory.set("pane-3:14", { top: 200, following: false });
    const initial: Activity[] = [
      { seq: 1, kind: "text", worker: "worker", ts: "", summary: "Before resize", intent_id: "14" },
    ];
    const grip = { title: "Move", onPointerDown: vi.fn(), resizing: false };
    const r = render(view(initial, { dockGrip: grip }));
    const viewport = screen.getByTestId("transcript").parentElement?.parentElement as HTMLDivElement;
    Object.defineProperties(viewport, {
      scrollHeight: { configurable: true, value: 1200 },
      clientHeight: { configurable: true, value: 300 },
    });
    act(() => vi.runOnlyPendingTimers());
    r.rerender(view(initial, { dockGrip: { ...grip, resizing: true } }));
    calls = [];
    callbacks.onFollowing.mockClear();
    const updated = [...initial, { ...initial[0], seq: 2, summary: "Arrived during resize" }];
    r.rerender(view(updated, { dockGrip: { ...grip, resizing: true } }));
    act(() => {
      observer?.([], {} as ResizeObserver);
      vi.runOnlyPendingTimers();
    });
    fireEvent.scroll(viewport);
    expect(calls).toEqual([]);
    expect(callbacks.onFollowing).not.toHaveBeenCalled();
    expect(screen.getByTestId("transcript")).not.toHaveTextContent("Arrived during resize");
    r.rerender(view(updated, { dockGrip: grip }));
    expect(screen.getByTestId("transcript")).toHaveTextContent("Arrived during resize");
    expect(calls).toEqual(following ? [1200] : []);
    expect(callbacks.onFollowing).not.toHaveBeenCalled();
  });
  it("offers a retry rather than an indefinite loader for failed history", () => {
    render(
      view([], {
        history: { items: [], loaded: false, loading: false, loadingMore: false, hasMore: false, error: "offline" },
      }),
    );
    expect(screen.getByText(/offline/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(callbacks.onRetry).toHaveBeenCalledOnce();
  });
});

it("distinguishes running, done and idle without losing the selected outline", () => {
  const r = render(view());
  const shell = document.querySelector("[data-live-pane]")!;
  expect(shell).toHaveAttribute("data-run-state", "running");
  expect(shell).toHaveAttribute("data-selected", "true");
  expect(shell.querySelector(".live-pane-status-ring")).toBeNull();
  r.rerender(view([], { session: { ...session, status: "done", live: false } }));
  expect(shell).toHaveAttribute("data-run-state", "done");
  expect(shell.querySelector(".live-pane-status-ring")).toBeNull();
  expect(shell).toHaveAttribute("data-selected", "true");
  r.rerender(view([], { session: { ...session, role: "planner", live: false } }));
  expect(shell).toHaveAttribute("data-run-state", "idle");
});
