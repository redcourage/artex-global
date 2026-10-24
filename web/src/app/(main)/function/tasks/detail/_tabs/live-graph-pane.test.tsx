import "@testing-library/jest-dom/vitest";
import * as React from "react";

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import messages from "../../../../../../../messages/en.json";
import { LiveGraphPane } from "./live-graph-pane";

const graph = vi.hoisted(() => vi.fn());
vi.mock("@/lib/api", () => ({ api: { explorationGraph: graph } }));
vi.mock("@/components/exploration-graph", () => ({
  ExplorationGraph: ({ nodes }: { nodes: { id: string }[] }) => {
    const [camera, setCamera] = React.useState("initial");
    return (
      <button type="button" onClick={() => setCamera("panned")}>
        {camera}:{nodes.map((n) => n.id).join(",")}
      </button>
    );
  },
}));
const view = (resizing = false) => (
  <NextIntlClientProvider locale="en" messages={messages}>
    <LiveGraphPane taskId="task" grip={{ title: "Move", onPointerDown: vi.fn(), resizing }} onClose={vi.fn()} />
  </NextIntlClientProvider>
);
beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(document, "hidden", "get").mockReturnValue(false);
  graph.mockReset().mockResolvedValue({ nodes: [{ id: "a" }], edges: [] });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});
it("updates without remounting the camera, defers rendering during resize, and stops polling on close", async () => {
  const r = render(view());
  await act(async () => {});
  fireEvent.click(screen.getByText("initial:a"));
  graph.mockResolvedValue({ nodes: [{ id: "b" }], edges: [] });
  r.rerender(view(true));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000);
  });
  expect(screen.getByText("panned:a")).toBeInTheDocument();
  r.rerender(view(false));
  expect(screen.getByText("panned:b")).toBeInTheDocument();
  r.unmount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(15000);
  });
  expect(graph).toHaveBeenCalledTimes(2);
});
it("retains the last graph after an error and retries without creating a second polling loop", async () => {
  render(view());
  await act(async () => {});
  graph.mockRejectedValue(new Error("offline"));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000);
  });
  expect(screen.getByText("initial:a")).toBeInTheDocument();
  expect(screen.getByText(messages.sessions.live.graphError)).toBeInTheDocument();
  graph.mockResolvedValue({ nodes: [{ id: "c" }], edges: [] });
  fireEvent.click(screen.getByRole("button", { name: messages.sessions.live.graphRetry }));
  await act(async () => {});
  expect(screen.getByText("initial:c")).toBeInTheDocument();
  expect(screen.queryByText(messages.sessions.live.graphError)).not.toBeInTheDocument();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000);
  });
  expect(graph).toHaveBeenCalledTimes(4);
});
it("does not overlap slow requests and ignores a result after unmount", async () => {
  let resolve!: (data: { nodes: never[]; edges: never[] }) => void;
  graph.mockImplementation(
    () =>
      new Promise((r) => {
        resolve = r;
      }),
  );
  const r = render(view());
  await act(async () => {
    await vi.advanceTimersByTimeAsync(15000);
  });
  expect(graph).toHaveBeenCalledTimes(1);
  r.unmount();
  await act(async () => {
    resolve({ nodes: [], edges: [] });
  });
  expect(screen.queryByText("initial:")).not.toBeInTheDocument();
});
