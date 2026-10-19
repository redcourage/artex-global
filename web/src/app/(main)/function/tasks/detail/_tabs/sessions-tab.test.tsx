import "@testing-library/jest-dom/vitest";

import { act, cleanup, render, screen } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "@/lib/api";
import type { Activity } from "@/lib/types";

import messages from "../../../../../../../messages/en.json";
import { SessionsTab } from "./sessions-tab";

vi.mock("@/lib/api", () => ({
  sseUrl: (path: string) => path,
  api: {
    mainSessions: vi.fn(),
    activityHistory: vi.fn(),
    tokenStats: vi.fn(),
    chatStatus: vi.fn(),
    taskLLMResolution: vi.fn(),
    interceptTask: vi.fn(),
    taskIntentAssets: vi.fn(),
    intentsPage: vi.fn(),
  },
}));
vi.mock("@/lib/mock/enabled", () => ({ MOCK: false }));
vi.mock("@/components/transcript", () => ({
  Transcript: ({ activity }: { activity: Activity[] }) => (
    <div data-testid="transcript">
      {activity.map((a) => (
        <div key={a.seq}>{a.summary}</div>
      ))}
    </div>
  ),
}));
vi.mock("@/hooks/use-side-questions", () => ({ useSideQuestions: () => ({}) }));
vi.mock("@/components/side-question-workspace", () => ({
  SideQuestionWorkspace: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  SideQuestionButton: () => null,
}));

class TestEventSource {
  static instances: TestEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  close = vi.fn();
  constructor(_url: string) {
    TestEventSource.instances.push(this);
  }
}

const activity = (seq: number, summary: string): Activity =>
  ({
    seq,
    summary,
    kind: "text",
    worker: "mainagent",
    main_seg: 0,
    ts: "2026-10-19T00:00:00Z",
  }) as Activity;
let persisted: Activity[];

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  TestEventSource.instances = [];
  vi.stubGlobal("EventSource", TestEventSource);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {
        /* Layout is irrelevant to transcript delivery. */
      }
      unobserve() {
        /* Layout is irrelevant to transcript delivery. */
      }
      disconnect() {
        /* No real observations in jsdom. */
      }
    },
  );
  persisted = [activity(1, "initial message")];
  vi.mocked(api.mainSessions).mockResolvedValue({ current: 0, sessions: [{ seq: 0, created_at: "" }] });
  vi.mocked(api.activityHistory).mockImplementation(async () => ({
    items: [...persisted],
    snapshotCursor: persisted.at(-1)?.seq ?? 0,
    earliestCursor: persisted[0]?.seq ?? 0,
    hasMore: false,
  }));
  vi.mocked(api.tokenStats).mockResolvedValue({ total: null, sessions: [] } as never);
  vi.mocked(api.chatStatus).mockResolvedValue({ running: false });
  vi.mocked(api.taskLLMResolution).mockResolvedValue(null as never);
  vi.mocked(api.interceptTask).mockResolvedValue([]);
  vi.mocked(api.taskIntentAssets).mockResolvedValue([]);
  vi.mocked(api.intentsPage).mockResolvedValue({ items: [], hasMore: false } as never);
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

async function show(taskId = "1") {
  const view = render(
    <NextIntlClientProvider locale="en" messages={messages}>
      <SessionsTab taskId={taskId} />
    </NextIntlClientProvider>,
  );
  await act(async () => {
    /* Flush the initial history request. */
  });
  return view;
}

describe("task transcript reconciliation", () => {
  it("shows persisted replies when SSE opens but delivers no events", async () => {
    await show();
    expect(screen.getByText("initial message")).toBeInTheDocument();
    act(() => TestEventSource.instances.at(-1)?.onopen?.());
    persisted.push(activity(2, "reply saved while the stream is silent"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(screen.getByText("reply saved while the stream is silent")).toBeInTheDocument();
  });

  it("merges a missed event even while other SSE events arrive, without duplicates", async () => {
    await show();
    const stream = TestEventSource.instances.at(-1)!;
    act(() => stream.onopen?.());
    const missed = activity(2, "missed user message");
    const delivered = activity(3, "live answer");
    persisted.push(missed, delivered);
    act(() => stream.onmessage?.({ data: JSON.stringify(delivered) }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(screen.getByText("missed user message")).toBeInTheDocument();
    expect(screen.getAllByText("live answer")).toHaveLength(1);
  });

  it("retries a failed history request and stops requesting after unmount", async () => {
    const view = await show();
    act(() => TestEventSource.instances.at(-1)?.onopen?.());
    vi.mocked(api.activityHistory).mockRejectedValueOnce(new Error("temporary disconnect"));
    persisted.push(activity(2, "reply recovered after request failure"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(screen.queryByText("reply recovered after request failure")).not.toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(screen.getByText("reply recovered after request failure")).toBeInTheDocument();
    view.unmount();
    const calls = vi.mocked(api.activityHistory).mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(api.activityHistory).toHaveBeenCalledTimes(calls);
  });
});
