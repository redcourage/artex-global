import "@testing-library/jest-dom/vitest";
import * as React from "react";

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type DockNode, dockGrid } from "@/lib/session-dock";

import messages from "../../messages/en.json";
import { SessionDockWorkspace } from "./session-dock-workspace";

const mounts = vi.fn();
const renders = vi.fn();
const changes = vi.fn();
const displayFrames = new Map<number, FrameRequestCallback>();
let displayFrame = 0;
function flushDisplayFrame() {
  act(() => {
    const callbacks = [...displayFrames.values()];
    displayFrames.clear();
    callbacks.forEach((callback) => callback(16));
  });
}
function movePointer(target: Element | Window, props: PointerEventInit) {
  fireEvent.pointerMove(target, props);
  flushDisplayFrame();
}
function Content({ id }: { id: string }) {
  renders(id);
  React.useEffect(() => {
    mounts(id);
  }, [id]);
  return <input aria-label={`Draft ${id}`} defaultValue="reading position stays" />;
}
function Example({ toolbarContainer, count = 3 }: { toolbarContainer?: HTMLElement; count?: number } = {}) {
  const ids = Array.from({ length: count }, (_, i) => String.fromCharCode(97 + i));
  const [tree, setTree] = React.useState<DockNode>(() => dockGrid(ids, count === 8 ? 4 : 3));
  return (
    <NextIntlClientProvider locale="en" messages={messages}>
      <button type="button" onClick={() => setTree(dockGrid(ids, 1))}>
        Reset external layout
      </button>
      <SessionDockWorkspace
        toolbarContainer={toolbarContainer}
        tree={tree}
        onChange={(value) => {
          changes(value);
          setTree(value);
        }}
        panes={ids.map((id) => ({
          id,
          label: id,
          render: (grip) => (
            <section>
              <button type="button" onPointerDown={grip.onPointerDown} title={grip.title}>
                {id}
              </button>
              <button type="button" onClick={grip.onToggleExpand} aria-expanded={grip.expanded}>
                Expand {id}
              </button>
              <div data-dock-resize-content>
                <Content id={id} />
              </div>
            </section>
          ),
        }))}
      />
    </NextIntlClientProvider>
  );
}
beforeEach(() => {
  displayFrames.clear();
  displayFrame = 0;
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    displayFrames.set(++displayFrame, callback);
    return displayFrame;
  });
  vi.stubGlobal("cancelAnimationFrame", (id: number) => displayFrames.delete(id));
  mounts.mockClear();
  renders.mockClear();
  changes.mockClear();
  class Pointer extends MouseEvent {
    pointerId: number;
    constructor(type: string, init: PointerEventInit = {}) {
      super(type, init);
      this.pointerId = init.pointerId ?? 1;
    }
  }
  vi.stubGlobal("PointerEvent", Pointer);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
function dropTarget() {
  vi.spyOn(document.querySelector("[data-dock-workspace]")!, "getBoundingClientRect").mockReturnValue({
    left: 0,
    top: 0,
    width: 900,
    height: 500,
  } as DOMRect);
  const target = document.querySelector('[data-dock-pane="b"]')!;
  vi.spyOn(target, "getBoundingClientRect").mockReturnValue({ left: 300, top: 0, width: 300, height: 500 } as DOMRect);
  Object.defineProperty(document, "elementFromPoint", { configurable: true, value: vi.fn(() => target) });
  return target as HTMLElement;
}
describe("dock gestures", () => {
  it("expands in place and restores layout, draft and scroll on Escape", () => {
    render(<Example count={8} />);
    const pane = document.querySelector<HTMLElement>('[data-dock-pane="c"]')!;
    const originalStyle = pane.getAttribute("style");
    const draft = screen.getByLabelText("Draft c");
    fireEvent.change(draft, { target: { value: "unsent chat" } });
    const content = draft.parentElement!;
    content.scrollTop = 125;
    const viewport = document.querySelector("[data-dock-workspace]")!.parentElement!;
    Object.defineProperties(viewport, { clientWidth: { value: 1000 }, clientHeight: { value: 700 } });
    const expand = screen.getByRole("button", { name: "Expand c" });
    expand.focus();
    fireEvent.click(expand);
    expect(pane).toHaveAttribute("data-dock-expanded", "true");
    expect(pane).toHaveStyle({ width: "920px", height: "644px" });
    expect(document.querySelector('[data-dock-pane="a"]')).toHaveAttribute("inert");
    expect(screen.queryAllByRole("separator")).toHaveLength(0);
    expect(screen.getByLabelText("Draft c")).toBe(draft);
    expect(draft).toHaveValue("unsent chat");
    expect(content.scrollTop).toBe(125);
    fireEvent.keyDown(window, { key: "Escape" });
    expect(pane.getAttribute("style")).toBe(originalStyle);
    expect(document.querySelector('[data-dock-pane="a"]')).not.toHaveAttribute("inert");
    expect(expand).toHaveFocus();
    expect(mounts).toHaveBeenCalledTimes(8);
    expect(changes).not.toHaveBeenCalled();
    expect(draft).toHaveValue("unsent chat");
  });
  it("restores when the blurred background is clicked", () => {
    render(<Example />);
    fireEvent.click(screen.getByRole("button", { name: "Expand a" }));
    const backdrop = document.querySelector<HTMLElement>("[data-dock-focus-backdrop]")!;
    expect(backdrop).toHaveStyle({ visibility: "visible", opacity: "1" });
    fireEvent.click(backdrop);
    expect(backdrop).toHaveStyle({ visibility: "hidden", opacity: "0" });
    expect(screen.getByRole("button", { name: "Expand a" })).toHaveAttribute("aria-expanded", "false");
    expect(changes).not.toHaveBeenCalled();
  });
  it("restores using its expand control without changing the tree", () => {
    render(<Example />);
    const button = screen.getByRole("button", { name: "Expand a" });
    fireEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(document.querySelector("[data-dock-focus-backdrop]")).toHaveStyle({ visibility: "hidden" });
    expect(changes).not.toHaveBeenCalled();
  });
  it("moves the controls into the shared toolbar without moving or remounting pane contents", () => {
    const toolbar = document.createElement("div");
    document.body.append(toolbar);
    const r = render(<Example />);
    const draft = screen.getByLabelText("Draft a");
    fireEvent.change(draft, { target: { value: "keep draft" } });
    r.rerender(<Example toolbarContainer={toolbar} />);
    expect(toolbar.querySelector("summary")).toHaveTextContent("Arrange panes");
    expect(screen.getByLabelText("Draft a")).toBe(draft);
    expect(draft).toHaveValue("keep draft");
    expect(mounts).toHaveBeenCalledTimes(3);
    expect(screen.queryByText(messages.sessions.live.dock.hint)).not.toBeInTheDocument();
    r.unmount();
    toolbar.remove();
  });
  it("moves using a pointer without remounting or clearing pane content", () => {
    render(<Example />);
    const original = screen.getByLabelText("Draft c");
    fireEvent.change(original, { target: { value: "keep this state" } });
    dropTarget();
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 700, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 10 });
    expect(screen.getByText("Above", { selector: "span" })).toBeInTheDocument();
    expect(document.querySelector('[data-dock-pane="b"]')).toHaveStyle({ height: "50%" });
    expect(document.querySelector("[data-dock-placeholder]")).toHaveStyle({ height: "50%" });
    expect(document.querySelector('[data-dock-pane="c"]')).toHaveAttribute("data-dock-dragging", "true");
    expect(changes).not.toHaveBeenCalled();
    fireEvent.pointerUp(window, { clientX: 450, clientY: 10 });
    expect(screen.getByLabelText("Draft c")).toBe(original);
    expect(original).toHaveValue("keep this state");
    expect(mounts).toHaveBeenCalledTimes(3);
    expect(document.querySelector('[data-dock-pane="c"]')).toHaveStyle({ height: "50%" });
    expect(screen.getByText("Pane moved")).toBeInTheDocument();
  });
  it("Escape cancels the preview without rearranging panes", () => {
    render(<Example />);
    dropTarget();
    const original = document.querySelector('[data-dock-pane="c"]')!.getAttribute("style");
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 700, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 10 });
    fireEvent.keyDown(window, { key: "Escape" });
    fireEvent.pointerUp(window);
    expect(document.querySelector('[data-dock-pane="c"]')).toHaveAttribute("style", original);
    expect(screen.queryByText("Above", { selector: "span" })).not.toBeInTheDocument();
  });
  it("slides neighbors using frozen hit regions without remounting", () => {
    render(<Example />);
    const target = dropTarget();
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 750, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 250 });
    const preview = target.getAttribute("style");
    expect(target.style.left).toBe(document.querySelector<HTMLElement>('[data-dock-pane="c"]')!.style.left);
    vi.mocked(target.getBoundingClientRect).mockReturnValue({ left: 600, top: 0, width: 300, height: 500 } as DOMRect);
    movePointer(window, { clientX: 451, clientY: 250 });
    expect(target).toHaveAttribute("style", preview);
    expect(changes).not.toHaveBeenCalled();
    fireEvent.pointerUp(window, { clientX: 451, clientY: 250 });
    expect(changes).toHaveBeenCalledTimes(1);
    expect(mounts).toHaveBeenCalledTimes(3);
  });
  it("uses the release position and cancels an outside drop", () => {
    render(<Example />);
    dropTarget();
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 750, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 10 });
    fireEvent.pointerUp(window, { clientX: 450, clientY: 490 });
    expect(document.querySelector('[data-dock-pane="c"]')).toHaveStyle({ top: "50%" });
    expect(changes).toHaveBeenCalledTimes(1);
    fireEvent.pointerDown(screen.getByRole("button", { name: "a" }), { button: 0, clientX: 100, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 10 });
    fireEvent.pointerUp(window, { clientX: 1100, clientY: 20 });
    expect(changes).toHaveBeenCalledTimes(1);
    expect(document.querySelector("[data-dock-placeholder]")).not.toBeInTheDocument();
  });
  it("ignores other pointers and small movements, and cancels on blur", () => {
    render(<Example />);
    dropTarget();
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), {
      button: 0,
      clientX: 750,
      clientY: 20,
      pointerId: 1,
    });
    movePointer(window, { clientX: 752, clientY: 21, pointerId: 1 });
    expect(document.querySelector("[data-dock-dragging]")).not.toBeInTheDocument();
    movePointer(window, { clientX: 450, clientY: 10, pointerId: 1 });
    fireEvent.pointerCancel(window, { pointerId: 2 });
    expect(document.querySelector("[data-dock-placeholder]")).toBeInTheDocument();
    fireEvent.blur(window);
    expect(document.querySelector("[data-dock-placeholder]")).not.toBeInTheDocument();
    fireEvent.pointerUp(window, { clientX: 450, clientY: 10, pointerId: 1 });
    expect(changes).not.toHaveBeenCalled();
  });
  it("cancels a drag when the owning screen replaces the layout", () => {
    render(<Example />);
    dropTarget();
    fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 750, clientY: 20 });
    movePointer(window, { clientX: 450, clientY: 10 });
    fireEvent.click(screen.getByRole("button", { name: "Reset external layout" }));
    expect(document.querySelector("[data-dock-placeholder]")).not.toBeInTheDocument();
    fireEvent.pointerUp(window, { clientX: 450, clientY: 10 });
    expect(changes).not.toHaveBeenCalled();
    expect(document.querySelector('[data-dock-pane="a"]')).toHaveStyle({ width: "100%" });
    expect(mounts).toHaveBeenCalledTimes(3);
  });
  it.each([
    "x",
    "y",
  ] as const)("previews %s resizing once per frame without rendering transcripts and commits only the release", (axis) => {
    let nextFrame = 0;
    const frames = new Map<number, FrameRequestCallback>();
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      frames.set(++nextFrame, callback);
      return nextFrame;
    });
    vi.stubGlobal("cancelAnimationFrame", (id: number) => frames.delete(id));
    render(<Example />);
    if (axis === "y") fireEvent.click(screen.getByRole("button", { name: "Reset external layout" }));
    const canvas = document.querySelector("[data-dock-workspace]")!;
    vi.spyOn(canvas, "getBoundingClientRect").mockReturnValue({ left: 0, top: 0, width: 1200, height: 900 } as DOMRect);
    const handle = screen.getAllByRole("separator")[0];
    const original = screen.getByLabelText("Draft a");
    fireEvent.pointerDown(handle, { button: 0, clientX: 400, clientY: 300 });
    const before = renders.mock.calls.length;
    for (let i = 0; i < 40; i++) fireEvent.pointerMove(window, { clientX: 440 + i, clientY: 340 + i });
    expect(frames.size).toBe(1);
    expect(changes).not.toHaveBeenCalled();
    act(() => {
      const callbacks = [...frames.values()];
      frames.clear();
      callbacks.forEach((callback) => callback(16));
    });
    const a = document.querySelector<HTMLElement>('[data-dock-pane="a"]')!;
    expect(parseFloat(axis === "x" ? a.style.width : a.style.height)).toBeCloseTo(axis === "x" ? 479 / 12 : 379 / 9);
    expect(renders).toHaveBeenCalledTimes(before);
    expect(screen.getByLabelText("Draft a")).toBe(original);
    fireEvent.pointerUp(window, { clientX: 600, clientY: 450 });
    expect(changes).toHaveBeenCalledTimes(1);
    expect(a).toHaveStyle(axis === "x" ? { width: "50%" } : { height: "50%" });
    expect(frames.size).toBe(0);
    expect(mounts).toHaveBeenCalledTimes(3);
  });
  it.each([
    false,
    true,
  ])("holds transcript line width during resize and restores it on release/cancel (%s)", (cancel) => {
    render(<Example />);
    const canvas = document.querySelector("[data-dock-workspace]")!;
    vi.spyOn(canvas, "getBoundingClientRect").mockReturnValue({ left: 0, top: 0, width: 1200, height: 900 } as DOMRect);
    const contents = Array.from(document.querySelectorAll<HTMLElement>("[data-dock-resize-content]"));
    for (const content of contents)
      vi.spyOn(content, "getBoundingClientRect").mockReturnValue({ width: 392, height: 460 } as DOMRect);
    contents[1].style.width = "95%";
    contents[0].setAttribute("data-dock-resize-height", "");
    contents[0].style.height = "100%";
    fireEvent.pointerDown(screen.getAllByRole("separator")[0], { button: 0, clientX: 400 });
    expect(contents.map((el) => el.style.width)).toEqual(["392px", "392px", "392px"]);
    expect(contents[0].style.height).toBe("460px");
    fireEvent.pointerMove(window, { clientX: 600 });
    expect(contents.map((el) => el.style.width)).toEqual(["392px", "392px", "392px"]);
    expect(contents[0].style.height).toBe("460px");
    if (cancel) fireEvent.keyDown(window, { key: "Escape" });
    else fireEvent.pointerUp(window, { clientX: 600 });
    expect(contents.map((el) => el.style.width)).toEqual(["", "95%", ""]);
    expect(contents[0].style.height).toBe("100%");
    expect(changes).toHaveBeenCalledTimes(cancel ? 0 : 1);
  });
  it("cancels a resize frame on Escape and when the owner changes the layout", () => {
    const pending = new Map<number, FrameRequestCallback>();
    let frame = 0;
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      pending.set(++frame, callback);
      return frame;
    });
    vi.stubGlobal("cancelAnimationFrame", (id: number) => pending.delete(id));
    render(<Example />);
    vi.spyOn(document.querySelector("[data-dock-workspace]")!, "getBoundingClientRect").mockReturnValue({
      left: 0,
      top: 0,
      width: 1200,
      height: 900,
    } as DOMRect);
    const handle = screen.getAllByRole("separator")[0];
    const original = document.querySelector<HTMLElement>('[data-dock-pane="a"]')!.style.width;
    fireEvent.pointerDown(handle, { button: 0 });
    fireEvent.pointerMove(window, { clientX: 600, clientY: 0 });
    act(() => {
      const callbacks = [...pending.values()];
      pending.clear();
      callbacks.forEach((callback) => callback(16));
    });
    fireEvent.keyDown(window, { key: "Escape" });
    expect(document.querySelector('[data-dock-pane="a"]')).toHaveStyle({ width: original });
    expect(changes).not.toHaveBeenCalled();
    fireEvent.pointerDown(handle, { button: 0 });
    fireEvent.pointerMove(window, { clientX: 600, clientY: 0 });
    fireEvent.click(screen.getByRole("button", { name: "Reset external layout" }));
    expect(pending.size).toBe(0);
    expect(changes).not.toHaveBeenCalled();
    expect(document.querySelector('[data-dock-pane="a"]')).toHaveStyle({ width: "100%" });
  });
  it("supports keyboard placement and keyboard resizing", () => {
    render(<Example />);
    fireEvent.click(screen.getByText("Arrange panes"));
    fireEvent.change(screen.getByLabelText("Move pane"), { target: { value: "c" } });
    fireEvent.change(screen.getByLabelText("Relative to"), { target: { value: "b" } });
    fireEvent.change(screen.getByLabelText("Position"), { target: { value: "bottom" } });
    fireEvent.click(screen.getByRole("button", { name: "Move", hidden: true }));
    const handle = screen.getAllByRole("separator").find((el) => el.getAttribute("aria-orientation") === "horizontal")!;
    expect(handle).toHaveAttribute("aria-valuenow", "50");
    fireEvent.keyDown(handle, { key: "ArrowDown" });
    expect(handle).toHaveAttribute("aria-valuenow", "55");
    expect(mounts).toHaveBeenCalledTimes(3);
  });
});

it("coalesces drag bursts without layout reads or content renders between target changes", () => {
  render(<Example />);
  dropTarget();
  const canvas = document.querySelector<HTMLElement>("[data-dock-workspace]")!;
  const read = vi.mocked(canvas.getBoundingClientRect);
  fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 750, clientY: 20 });
  for (let i = 0; i < 80; i++) fireEvent.pointerMove(window, { clientX: 400 + i, clientY: 10 });
  expect(displayFrames.size).toBe(1);
  expect(document.querySelector("[data-dock-dragging]")).not.toBeInTheDocument();
  flushDisplayFrame();
  const count = renders.mock.calls.length;
  const measured = read.mock.calls.length;
  for (let i = 0; i < 80; i++) fireEvent.pointerMove(window, { clientX: 401 + i, clientY: 11 });
  expect(displayFrames.size).toBe(1);
  flushDisplayFrame();
  expect(renders).toHaveBeenCalledTimes(count);
  expect(read).toHaveBeenCalledTimes(measured);
  expect(canvas.style.getPropertyValue("--dock-drag-x")).toBe("");
  expect(document.querySelector<HTMLElement>('[data-dock-pane="c"]')!.style.getPropertyValue("--dock-drag-x")).toBe(
    "-270px",
  );
  fireEvent.pointerUp(window, { clientX: 450, clientY: 490 });
  expect(displayFrames.size).toBe(0);
  expect(document.querySelector('[data-dock-pane="c"]')).toHaveStyle({ top: "50%" });
  expect(changes).toHaveBeenCalledTimes(1);
});

it("animates neighbor transforms instead of animating layout dimensions", () => {
  render(<Example />);
  dropTarget();
  const animate = vi.fn(() => ({ cancel: vi.fn(), onfinish: null }));
  for (const node of document.querySelectorAll<HTMLElement>("[data-dock-pane]")) {
    Object.defineProperty(node, "animate", { configurable: true, value: animate });
    vi.spyOn(node, "getBoundingClientRect").mockImplementation(
      () =>
        ({
          left: parseFloat(node.style.left) * 9,
          top: parseFloat(node.style.top) * 5,
          width: parseFloat(node.style.width) * 9,
          height: parseFloat(node.style.height) * 5,
        }) as DOMRect,
    );
  }
  fireEvent.pointerDown(screen.getByRole("button", { name: "c" }), { button: 0, clientX: 750, clientY: 20 });
  movePointer(window, { clientX: 450, clientY: 10 });
  expect(animate).toHaveBeenCalled();
  for (const call of animate.mock.calls as unknown as [Keyframe[], KeyframeAnimationOptions][]) {
    expect(call[0].every((frame) => Object.keys(frame).every((key) => key === "transform"))).toBe(true);
  }
  fireEvent.keyDown(window, { key: "Escape" });
  expect(document.querySelector('[data-dock-pane="c"]')).toHaveStyle({ height: "100%" });
  expect(changes).not.toHaveBeenCalled();
});

it("keeps eight mounted panes responsive through repeated drag, docking, cancellation, and resize bursts", () => {
  render(<Example count={8} />);
  const canvas = document.querySelector<HTMLElement>("[data-dock-workspace]")!;
  const measure = vi
    .spyOn(canvas, "getBoundingClientRect")
    .mockReturnValue({ left: 0, top: 0, width: 1920, height: 1080 } as DOMRect);
  const inputs = Array.from(document.querySelectorAll("input"));
  for (const input of inputs) fireEvent.change(input, { target: { value: "keep eight live drafts" } });
  const content = document.querySelector<HTMLElement>("[data-dock-resize-content]")!;
  content.setAttribute("data-dock-resize-height", "");
  vi.spyOn(content, "getBoundingClientRect").mockReturnValue({ width: 472, height: 490 } as DOMRect);
  // Cross several destination zones on every drag. Pixel motion within a zone
  // never renders the transcript fixtures or remeasures the workspace.
  for (let round = 0; round < 12; round++) {
    fireEvent.pointerDown(screen.getByRole("button", { name: "h" }), { button: 0, clientX: 1700, clientY: 600 });
    const measurements = measure.mock.calls.length;
    for (let zone = 0; zone < 3; zone++) {
      for (let i = 0; i < 80; i++) fireEvent.pointerMove(window, { clientX: 60 + zone * 480 + i, clientY: 12 });
      expect(displayFrames.size).toBe(1);
      flushDisplayFrame();
      const renderCount = renders.mock.calls.length;
      for (let i = 0; i < 80; i++) fireEvent.pointerMove(window, { clientX: 60 + zone * 480 + i, clientY: 13 });
      flushDisplayFrame();
      expect(renders).toHaveBeenCalledTimes(renderCount);
      expect(measure).toHaveBeenCalledTimes(measurements);
    }
    if (round % 2) fireEvent.keyDown(window, { key: "Escape" });
    else fireEvent.pointerUp(window, { clientX: 900, clientY: 25 });
    expect(displayFrames.size).toBe(0);
    expect(content.style.width).toBe("");
    expect(content.style.height).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Reset external layout" }));
  }
  measure.mockReturnValue({ left: 0, top: 0, width: 1920, height: 2400 } as DOMRect);
  const separator = screen.getAllByRole("separator")[0];
  fireEvent.pointerDown(separator, { button: 0, clientX: 100, clientY: 135 });
  const rendersBeforeResize = renders.mock.calls.length;
  for (let i = 0; i < 160; i++) fireEvent.pointerMove(window, { clientX: 100, clientY: 140 + i });
  expect(displayFrames.size).toBe(1);
  flushDisplayFrame();
  expect(renders).toHaveBeenCalledTimes(rendersBeforeResize);
  fireEvent.pointerUp(window, { clientX: 100, clientY: 310 });
  expect(displayFrames.size).toBe(0);
  expect(mounts).toHaveBeenCalledTimes(8);
  expect(changes).toHaveBeenCalledTimes(7);
  inputs.forEach((input) => {
    expect(input).toBeInTheDocument();
    expect(input).toHaveValue("keep eight live drafts");
  });
});

it.each(["finish", "reopen", "cancel"])("closes without scaling text and releases frozen content on %s", (action) => {
  render(<Example />);
  const pane = document.querySelector<HTMLElement>('[data-dock-pane="a"]')!;
  const viewport = document.querySelector("[data-dock-workspace]")!.parentElement!;
  Object.defineProperties(viewport, { clientWidth: { value: 1000 }, clientHeight: { value: 700 } });
  vi.spyOn(pane, "getBoundingClientRect").mockImplementation(
    () =>
      ({
        left: pane.dataset.dockExpanded ? 40 : 0,
        top: pane.dataset.dockExpanded ? 28 : 0,
        width: pane.dataset.dockExpanded ? 920 : 333,
        height: pane.dataset.dockExpanded ? 644 : 700,
      }) as DOMRect,
  );
  const activeAnimations: {
    cancel: ReturnType<typeof vi.fn>;
    onfinish: (() => void) | null;
    oncancel: (() => void) | null;
  }[] = [];
  const animate = vi.fn((_frames: Keyframe[], _options: KeyframeAnimationOptions) => {
    const animation = { cancel: vi.fn(), onfinish: null, oncancel: null };
    activeAnimations.push(animation);
    return animation;
  });
  Object.defineProperty(pane, "animate", { configurable: true, value: animate });
  const content = pane.querySelector<HTMLElement>("[data-dock-resize-content]")!;
  Object.defineProperties(content, { offsetWidth: { value: 912 }, offsetHeight: { value: 560 } });
  const draft = screen.getByLabelText("Draft a");
  fireEvent.change(draft, { target: { value: "preserve my draft" } });
  const button = screen.getByRole("button", { name: "Expand a" });
  fireEvent.click(button);
  act(() => activeAnimations[0].onfinish?.());
  fireEvent.click(button);
  const frames = animate.mock.calls.at(-1)![0];
  expect(frames.every((frame) => !String(frame.transform).includes("scale"))).toBe(true);
  expect(frames[0]).toMatchObject({ width: "920px", height: "644px" });
  expect(frames[1]).toMatchObject({ width: "333px", height: "700px" });
  expect(content).toHaveStyle({ width: "912px", height: "560px", flex: "0 0 auto" });
  if (action === "reopen") fireEvent.click(button);
  else act(() => (action === "finish" ? activeAnimations.at(-1)?.onfinish?.() : activeAnimations.at(-1)?.oncancel?.()));
  expect(content.style.width).toBe("");
  expect(content.style.height).toBe("");
  expect(content.style.flex).toBe("");
  expect(screen.getByLabelText("Draft a")).toBe(draft);
  expect(draft).toHaveValue("preserve my draft");
  expect(mounts).toHaveBeenCalledTimes(3);
  expect(changes).not.toHaveBeenCalled();
});
