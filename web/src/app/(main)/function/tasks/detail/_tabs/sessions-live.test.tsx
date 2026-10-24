import type * as React from "react";

import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Activity } from "@/lib/types";

import messages from "../../../../../../../messages/en.json";
import { SessionsTab } from "./sessions-tab";

const mock = vi.hoisted(() => ({
  history: vi.fn(),
  intents: vi.fn(),
  chat: vi.fn(),
  stop: vi.fn(),
  upload: vi.fn(),
  mainSessions: vi.fn(),
  graph: vi.fn(),
}));
vi.mock("@/lib/mock/enabled", () => ({ MOCK: false }));
vi.mock("@/lib/api", () => ({
  sseUrl: (url: string) => url,
  api: {
    mainSessions: mock.mainSessions,
    explorationGraph: mock.graph,
    chat: mock.chat,
    stopChat: mock.stop,
    chatUpload: mock.upload,
    activityHistory: mock.history,
    intentsPage: mock.intents,
    taskIntentAssets: async () => [],
    interceptTask: async () => [],
    tokenStats: async () => ({ total: null, sessions: [] }),
    chatStatus: async () => ({ running: false }),
    taskLLMResolution: async () => null,
  },
}));
vi.mock("@/components/exploration-graph", () => ({ ExplorationGraph: () => <div data-testid="graph-canvas" /> }));
vi.mock("@/components/transcript", () => ({
  Transcript: ({ activity }: { activity: Activity[] }) => (
    <div data-testid="transcript">{activity.map((item) => item.summary).join("\n")}</div>
  ),
}));
vi.mock("@/components/approval-execution-focus", () => ({
  useApprovalFocus: () => ({ state: null, close: vi.fn() }),
  useApprovalHistory: () => ({ ready: false }),
  ApprovalExecutionFocus: () => null,
}));
vi.mock("@/hooks/use-side-questions", () => ({
  useSideQuestions: () => ({ enabled: false, handleCommand: () => false }),
}));
vi.mock("@/components/side-question-workspace", () => ({
  SideQuestionButton: () => null,
  SideQuestionWorkspace: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/components/todo-popover", () => ({ TodoPopover: () => null }));
vi.mock("@/components/mention-textarea", () => ({
  MentionTextarea: (props: {
    value: string;
    onValueChange: (value: string) => void;
    placeholder?: string;
    "aria-label"?: string;
    disabled?: boolean;
    onKeyDown?: React.KeyboardEventHandler<HTMLTextAreaElement>;
  }) => (
    <textarea
      aria-label={props["aria-label"]}
      disabled={props.disabled}
      onKeyDown={props.onKeyDown}
      value={props.value}
      onChange={(e) => props.onValueChange(e.target.value)}
      placeholder={props.placeholder}
    />
  ),
}));

class FakeStream {
  static instances: FakeStream[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  close = vi.fn();
  constructor(public url: string) {
    FakeStream.instances.push(this);
  }
  emit(activity: Activity) {
    this.onmessage?.({ data: JSON.stringify(activity) });
  }
}
function view(live = true, autoFollow = false) {
  return (
    <NextIntlClientProvider locale="en" messages={messages}>
      <SessionsTab taskId="task-1" initialAutoFollow={autoFollow} liveMode={live} onLiveModeChange={vi.fn()} />
    </NextIntlClientProvider>
  );
}
const pane = (n: number) => screen.getByRole("region", { name: `Session pane ${n}` });
beforeEach(() => {
  FakeStream.instances = [];
  vi.spyOn(document, "hidden", "get").mockReturnValue(false);
  mock.mainSessions.mockReset().mockResolvedValue({ current: 3, sessions: [{ seq: 3, created_at: "" }] });
  mock.chat.mockReset().mockResolvedValue({ mode: "command" });
  mock.stop.mockReset().mockResolvedValue({});
  mock.upload
    .mockReset()
    .mockResolvedValue({ attachments: [{ path: "uploads/evidence.txt", name: "evidence.txt", size: 42 }] });
  mock.graph.mockReset().mockResolvedValue({ nodes: [], edges: [] });
  mock.history.mockReset().mockImplementation(async (_task: string, key: string) => ({
    items: [
      { seq: 1, kind: "text", worker: key === "plan" ? "planner" : "mainagent", summary: `History ${key}`, ts: "" },
    ],
    snapshotCursor: 10,
    hasMore: false,
  }));
  mock.intents.mockReset().mockResolvedValue({
    items: [14, 13, 12, 11].map((id) => ({
      id: String(id),
      kind: "intent",
      state: "running",
      payload: `Worker objective ${id}`,
      ts: "",
    })),
    hasMore: false,
  });
  vi.stubGlobal("EventSource", FakeStream);
  vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe = vi.fn();
      unobserve = vi.fn();
      disconnect = vi.fn();
    },
  );
  Object.defineProperty(HTMLElement.prototype, "scrollTo", {
    configurable: true,
    value(options: ScrollToOptions) {
      this.scrollTop = options.top ?? 0;
    },
  });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("live sessions sharing the task stream", () => {
  it("loads only visible histories with one SSE, renders concurrent frames, preserves layout and drafts across modes", async () => {
    const r = render(view());
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:14"));
    expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History main:3");
    expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History plan");
    expect(FakeStream.instances).toHaveLength(1);
    expect(mock.history.mock.calls.filter((call) => call[1] === "plan")).toHaveLength(1);
    expect(mock.history.mock.calls.some((call) => call[1] === "intent:13")).toBe(false);
    act(() => {
      FakeStream.instances[0].onopen?.();
      FakeStream.instances[0].emit({
        seq: 11,
        kind: "text",
        worker: "planner",
        ts: new Date().toISOString(),
        summary: "Planning in parallel",
      });
      FakeStream.instances[0].emit({
        seq: 12,
        kind: "text",
        worker: "worker",
        intent_id: "14",
        ts: new Date().toISOString(),
        summary: "Fresh evidence",
      });
    });
    expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("Planning in parallel");
    expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("Fresh evidence");
    fireEvent.click(screen.getByRole("button", { name: "Session layout" }));
    fireEvent.click(screen.getByRole("button", { name: "3 columns × 2 rows" }));
    await waitFor(() => expect(within(pane(4)).getByTestId("transcript")).toHaveTextContent("History intent:13"));
    expect(screen.getAllByRole("region", { name: /Session pane/ })).toHaveLength(6);
    fireEvent.click(within(pane(6)).getByRole("button", { name: "Pin session" }));
    fireEvent.click(screen.getByRole("button", { name: "2 columns × 2 rows" }));
    expect(screen.getAllByRole("region", { name: /Session pane/ })).toHaveLength(6);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    r.rerender(view(false));
    const composer = screen.getByRole("textbox");
    fireEvent.change(composer, { target: { value: "Keep this draft" } });
    r.rerender(view(true));
    expect(screen.getAllByRole("region", { name: /Session pane/ })).toHaveLength(6);
    expect(FakeStream.instances).toHaveLength(1);
    r.rerender(view(false));
    expect(screen.getByRole("textbox")).toHaveValue("Keep this draft");
    r.unmount();
    expect(FakeStream.instances[0].close).toHaveBeenCalledOnce();
  });
  it("restores the main composer, sends to the visible segment, respects IME, and stops a running turn", async () => {
    mock.chat.mockResolvedValue({ mode: "llm" });
    render(view());
    await waitFor(() => expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History main:3"));
    expect(within(pane(2)).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(pane(3)).queryByRole("textbox")).not.toBeInTheDocument();
    const input = within(pane(1)).getByRole("textbox", { name: "Send a message to the main agent" });
    fireEvent.change(input, { target: { value: "조사해줘" } });
    fireEvent.keyDown(input, { key: "Enter", isComposing: true });
    fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
    expect(mock.chat).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(mock.chat).toHaveBeenCalledWith("조사해줘", "task-1", undefined, 3));
    expect(input).toHaveValue("");
    const stop = await within(pane(1)).findByRole("button", { name: "Stop the current run" });
    fireEvent.click(stop);
    await waitFor(() => expect(mock.stop).toHaveBeenCalledWith("task-1"));
    expect(FakeStream.instances).toHaveLength(1);
  });

  it("keeps drafts and failed sends with their main segment when a pane changes sessions", async () => {
    mock.mainSessions.mockResolvedValue({
      current: 3,
      sessions: [
        { seq: 3, created_at: "" },
        { seq: 2, created_at: "" },
      ],
    });
    let rejectSend!: (error: Error) => void;
    mock.chat.mockImplementation(
      () =>
        new Promise((_resolve, reject) => {
          rejectSend = reject;
        }),
    );
    const r = render(view());
    await waitFor(() => expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History main:3"));
    const list = document.querySelector("[data-session-list]") as HTMLElement;
    fireEvent.change(within(pane(1)).getByRole("textbox"), { target: { value: "segment three draft" } });
    fireEvent.click(within(pane(3)).getByRole("button", { name: "Select pane 3" }));
    fireEvent.click(within(list).getByRole("button", { name: /^Main agent · session #3/ }));
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History main:2"));
    fireEvent.change(within(pane(3)).getByRole("textbox"), { target: { value: "segment two message" } });
    fireEvent.click(within(pane(3)).getByRole("button", { name: "Send message" }));
    expect(mock.chat).toHaveBeenCalledWith("segment two message", "task-1", undefined, 2);
    // The task-wide send guard also disables a different main segment's composer.
    expect(within(pane(1)).getByRole("textbox")).toBeDisabled();
    fireEvent.click(within(pane(3)).getByRole("button", { name: "Select pane 3" }));
    fireEvent.click(within(list).getByRole("button", { name: /^(?!Open ).*Worker objective 13/ }));
    await act(async () => rejectSend(new Error("retry me")));
    expect(within(pane(1)).getByRole("textbox")).toHaveValue("segment three draft");
    fireEvent.click(within(list).getByRole("button", { name: /^Main agent · session #3/ }));
    await waitFor(() => expect(within(pane(3)).getByRole("textbox")).toHaveValue("segment two message"));
    r.rerender(view(false));
    expect(screen.getByRole("textbox")).toHaveValue("segment three draft");
    expect(mock.chat).toHaveBeenCalledTimes(1);
  });

  it("uploads and sends attachments from the main pane", async () => {
    render(view());
    await waitFor(() => expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History main:3"));
    const file = new File(["evidence"], "evidence.txt", { type: "text/plain" });
    fireEvent.change(pane(1).querySelector('input[type="file"]')!, { target: { files: [file] } });
    await within(pane(1)).findByText("evidence.txt");
    expect(mock.upload).toHaveBeenCalledWith("task", "task-1", [file]);
    fireEvent.click(within(pane(1)).getByRole("button", { name: "Send message" }));
    await waitFor(() =>
      expect(mock.chat).toHaveBeenCalledWith(
        "",
        "task-1",
        [{ path: "uploads/evidence.txt", name: "evidence.txt", size: 42 }],
        3,
      ),
    );
  });

  it("switches the selected unpinned pane from the original session list without replacing pinned panes", async () => {
    render(view());
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:14"));
    fireEvent.click(within(pane(3)).getByRole("button", { name: "Select pane 3" }));
    fireEvent.click(screen.getByRole("button", { name: /^(?!Open ).*Worker objective 13/ }));
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:13"));
    expect(within(pane(2)).getByRole("button", { name: "Pin session" })).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(within(pane(1)).getByRole("button", { name: "Pin session" }));
    fireEvent.click(within(pane(1)).getByRole("button", { name: "Select pane 1" }));
    fireEvent.click(screen.getByRole("button", { name: /^(?!Open ).*Worker objective 12/ }));
    expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History main:3");
    expect(FakeStream.instances).toHaveLength(1);
  });
  it("clears pane selection on outside clicks without losing content or silently replacing an unselected pane", async () => {
    render(view());
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:14"));
    const select = within(pane(3)).getByRole("button", { name: "Select pane 3" });
    expect(select).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(select);
    expect(select).toHaveAttribute("aria-pressed", "true");
    expect(pane(3)).toHaveAttribute("data-selected", "true");
    const picker = document.createElement("div");
    picker.dataset.livePaneControls = "";
    document.body.append(picker);
    fireEvent.pointerDown(picker);
    expect(select).toHaveAttribute("aria-pressed", "true");
    picker.remove();
    fireEvent.pointerDown(document.body);
    expect(select).toHaveAttribute("aria-pressed", "false");
    expect(pane(3)).not.toHaveAttribute("data-selected");
    fireEvent.click(screen.getByRole("button", { name: /^(?!Open ).*Worker objective 13/ }));
    expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:14");
    fireEvent.click(select);
    fireEvent.pointerDown(screen.getByRole("button", { name: /^(?!Open ).*Worker objective 13/ }));
    expect(select).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: /^(?!Open ).*Worker objective 13/ }));
    await waitFor(() => expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:13"));
    expect(FakeStream.instances).toHaveLength(1);
  });
  it("polls every visible session during a connection outage and stops polling hidden panes", async () => {
    vi.useFakeTimers();
    const r = render(view());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(FakeStream.instances).toHaveLength(1);
    mock.history.mockClear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2050);
    });
    expect(new Set(mock.history.mock.calls.map((call) => call[1]))).toEqual(new Set(["main:3", "plan", "intent:14"]));
    r.rerender(view(false));
    mock.history.mockClear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2050);
    });
    expect(new Set(mock.history.mock.calls.map((call) => call[1]))).toEqual(new Set(["main:3"]));
    act(() => FakeStream.instances[0].onopen?.());
    mock.history.mockClear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2050);
    });
    expect(mock.history).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(new Set(mock.history.mock.calls.map((call) => call[1]))).toEqual(new Set(["main:3"]));
  });
  it("repairs missed events in every visible pane even while SSE stays connected", async () => {
    vi.useFakeTimers();
    render(view());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    act(() => FakeStream.instances[0].onopen?.());
    mock.history.mockClear().mockImplementation(async (_task: string, key: string) => ({
      items: [{ seq: 2, kind: "text", worker: "mainagent", summary: `Recovered ${key}`, ts: "" }],
      snapshotCursor: 10,
      hasMore: false,
    }));
    act(() =>
      FakeStream.instances[0].emit({
        seq: 3,
        kind: "text",
        worker: "mainagent",
        main_seg: 3,
        summary: "Later live event",
        ts: "",
      } as Activity),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5050);
    });
    expect(new Set(mock.history.mock.calls.map((call) => call[1]))).toEqual(new Set(["main:3", "plan", "intent:14"]));
    expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("Recovered main:3");
    expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("Later live event");
    expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("Recovered plan");
    expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("Recovered intent:14");
  });
});

describe("dynamic session panes", () => {
  function dragSession(id: string, x = 890, y = 250) {
    const canvas = document.querySelector("[data-dock-workspace]")!;
    vi.spyOn(canvas, "getBoundingClientRect").mockReturnValue({ left: 0, top: 0, width: 900, height: 500 } as DOMRect);
    const dataTransfer = { types: ["application/x-artex-session"], getData: () => id, dropEffect: "none" };
    const send = (type: string) => {
      const event = new Event(type, { bubbles: true, cancelable: true });
      Object.assign(event, { clientX: x, clientY: y, dataTransfer });
      fireEvent(canvas, event);
    };
    send("dragover");
    expect(document.querySelector("[data-dock-incoming]")).toBeInTheDocument();
    send("drop");
    expect(document.querySelector("[data-dock-incoming]")).not.toBeInTheDocument();
  }
  it("adds from Workers beyond six panes, keeps existing content, and closes/reopens the last pane", async () => {
    mock.intents.mockResolvedValue({
      items: [20, 19, 18, 17, 16, 15, 14, 13].map((id) => ({
        id: String(id),
        kind: "intent",
        state: "running",
        payload: `Worker objective ${id}`,
        ts: "",
      })),
      hasMore: false,
    });
    render(view());
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Open Worker objective 19 in a pane" })).toBeInTheDocument(),
    );
    const original = document.querySelector('[data-dock-pane="pane-1"]');
    dragSession("19", 450, 5);
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(4);
    expect(document.querySelector('[data-dock-pane="pane-7"]')).toHaveStyle({ height: "50%" });
    for (const id of [18, 17, 16, 15])
      fireEvent.click(screen.getByRole("button", { name: `Open Worker objective ${id} in a pane` }));
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(8);
    expect(document.querySelector('[data-dock-pane="pane-1"]')).toBe(original);
    // Reopening an already visible session focuses it, without duplicate subscriptions/panes.
    fireEvent.click(screen.getByRole("button", { name: "Open Worker objective 19 in a pane" }));
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(8);
    fireEvent.click(screen.getAllByRole("button", { name: messages.sessions.live.closePane })[3]);
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(7);
    expect(document.querySelector('[data-dock-pane="pane-1"]')).toBe(original);
    for (let i = 0; i < 7; i++)
      fireEvent.click(screen.getAllByRole("button", { name: messages.sessions.live.closePane })[0]);
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(0);
    expect(screen.getByText(messages.sessions.live.emptyWorkspace)).toBeInTheDocument();
    expect(FakeStream.instances[0].close).not.toHaveBeenCalled();
    expect(mock.stop).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Open Worker objective 19 in a pane" }));
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(1);
    expect(document.querySelector("[data-dock-pane]")).toHaveStyle({ width: "100%", height: "100%" });
  });
  it("rejects drops with unknown session ids and leaves the layout intact on drag cancellation", async () => {
    render(view());
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Open Worker objective 13 in a pane" })).toBeInTheDocument(),
    );
    dragSession("missing-session");
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(3);
    const source = Array.from(document.querySelectorAll('[draggable="true"]')).find((el) =>
      el.textContent?.includes("Worker objective 13"),
    )!;
    const transfer = { setData: vi.fn(), effectAllowed: "" };
    fireEvent.dragStart(source, { dataTransfer: transfer });
    expect(transfer.setData).toHaveBeenCalledWith("application/x-artex-session", "13");
    fireEvent.dragEnd(window);
    expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(3);
  });
});

it("starts with sessions only and docks an optional graph without replacing sessions", async () => {
  render(view());
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Open Worker objective 13 in a pane" })).toBeInTheDocument(),
  );
  expect(mock.graph).not.toHaveBeenCalled();
  expect(document.querySelector("[data-live-graph]")).not.toBeInTheDocument();
  const main = document.querySelector('[data-dock-pane="pane-1"]');
  fireEvent.click(screen.getByRole("button", { name: messages.sessions.live.graphAdd }));
  await screen.findByTestId("graph-canvas");
  expect(mock.graph).toHaveBeenCalledWith("task-1");
  expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(4);
  expect(document.querySelector('[data-dock-pane="pane-1"]')).toBe(main);
  fireEvent.click(screen.getByRole("button", { name: messages.sessions.live.layout }));
  fireEvent.click(screen.getByRole("button", { name: "2 columns × 1 rows" }));
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(2);
  expect(screen.getByTestId("graph-canvas")).toBeInTheDocument();
  for (let i = 0; i < 2; i++)
    fireEvent.click(screen.getAllByRole("button", { name: messages.sessions.live.closePane })[0]);
  expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(1);
  expect(screen.getByTestId("graph-canvas")).toBeInTheDocument();
  fireEvent.click(
    within(document.querySelector("[data-live-graph]") as HTMLElement).getByRole("button", {
      name: messages.sessions.live.graphClose,
    }),
  );
  expect(screen.getByText(messages.sessions.live.emptyWorkspace)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: messages.sessions.live.graphAdd }));
  await screen.findByTestId("graph-canvas");
  fireEvent.click(screen.getByRole("button", { name: "Open Worker objective 13 in a pane" }));
  expect(document.querySelectorAll("[data-dock-pane]")).toHaveLength(2);
});

it("keeps the stream and other panes mounted while focused, and recovers when the focused pane is closed", async () => {
  render(view());
  await waitFor(() =>
    expect(screen.getAllByRole("button", { name: messages.sessions.live.expandPane })).toHaveLength(3),
  );
  const panes = Array.from(document.querySelectorAll("[data-live-pane]"));
  fireEvent.click(screen.getAllByRole("button", { name: messages.sessions.live.expandPane })[0]);
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(3);
  expect(Array.from(document.querySelectorAll("[data-live-pane]"))).toEqual(panes);
  expect(FakeStream.instances[0].close).not.toHaveBeenCalled();
  fireEvent.click(
    within(document.querySelector("[data-dock-expanded]") as HTMLElement).getByRole("button", {
      name: messages.sessions.live.closePane,
    }),
  );
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(2);
  expect(document.querySelector("[data-dock-expanded]")).toBeNull();
  expect(screen.getAllByRole("button", { name: messages.sessions.live.expandPane })).toHaveLength(2);
  expect(document.querySelector("[data-dock-focus-backdrop]")).toHaveStyle({ visibility: "hidden" });
  expect(FakeStream.instances[0].close).not.toHaveBeenCalled();
});

it("automatically opens Planner then two running workers, rotating only completed panes", async () => {
  vi.useFakeTimers();
  mock.intents.mockResolvedValue({ items: [], hasMore: false });
  render(view(true, true));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(within(pane(1)).getByTestId("transcript")).toHaveTextContent("History plan");
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(1);
  function expectEqualWidths(count: number) {
    const nodes = Array.from(document.querySelectorAll<HTMLElement>("[data-dock-pane]"));
    expect(nodes).toHaveLength(count);
    for (const node of nodes) expect(parseFloat(node.style.width)).toBeCloseTo(100 / count);
  }
  expectEqualWidths(1);
  const planner = document.querySelector('[data-live-pane="pane-1"]');
  async function snapshot(states: [number, string][]) {
    mock.intents.mockResolvedValue({
      items: states.map(([id, state]) => ({
        id: String(id),
        kind: "intent",
        state,
        payload: `Worker objective ${id}`,
        ts: "",
      })),
      hasMore: false,
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
  }
  await snapshot([[11, "running"]]);
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(2);
  expectEqualWidths(2);
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:11");
  await snapshot([
    [13, "running"],
    [12, "running"],
    [11, "running"],
  ]);
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(3);
  expectEqualWidths(3);
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:11");
  expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:12");
  await snapshot([
    [13, "running"],
    [12, "running"],
    [11, "done"],
  ]);
  expect(pane(2)).toHaveAttribute("data-run-state", "done");
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:11");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1700);
  });
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:11");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(101);
  });
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:13");
  expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:12");
  await snapshot([
    [14, "open"],
    [13, "done"],
    [12, "done"],
    [11, "done"],
  ]);
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:13");
  expect(within(pane(3)).getByTestId("transcript")).toHaveTextContent("History intent:12");
  expect(pane(2)).toHaveAttribute("data-run-state", "done");
  expect(pane(3)).toHaveAttribute("data-run-state", "done");
  expect(document.querySelector('[data-live-pane="pane-1"]')).toBe(planner);
  expect(FakeStream.instances).toHaveLength(1);
  expect(FakeStream.instances[0].close).not.toHaveBeenCalled();
  fireEvent.click(within(pane(2)).getByRole("button", { name: messages.sessions.live.closePane }));
  expect(screen.getByRole("button", { name: messages.sessions.live.autoFollow })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
  await snapshot([
    [14, "running"],
    [13, "done"],
    [12, "done"],
  ]);
  expect(document.querySelectorAll("[data-live-pane]")).toHaveLength(2);
  fireEvent.click(screen.getByRole("button", { name: messages.sessions.live.autoFollow }));
  await act(async () => {});
  expect(within(pane(2)).getByTestId("transcript")).toHaveTextContent("History intent:14");
});
