import { describe, expect, it } from "vitest";

import {
  type DockZone,
  dockDropTarget,
  dockGeometry,
  dockGrid,
  dockIds,
  dockMinimum,
  dockMove,
  dockPreview,
  dockResize,
  dockResizeWithin,
  dockZone,
} from "./session-dock";

describe("panel docking", () => {
  it("supports mixed panels with independent minimum sizes", () => {
    const tree = dockGrid(["graph", "broadcast", "session"], 3);
    const sizes = { graph: { width: 600, height: 400 }, broadcast: { width: 280, height: 240 } };
    expect(dockMinimum(tree, sizes)).toEqual({ width: 1800, height: 400 });
    const moved = dockMove(tree, "graph", "broadcast", "top");
    expect(dockIds(moved).sort()).toEqual(["broadcast", "graph", "session"]);
    const minimum = dockMinimum(moved, sizes);
    const geometry = dockGeometry(moved);
    expect(geometry.panes.graph.width * minimum.width).toBeGreaterThanOrEqual(600);
    expect(geometry.panes.graph.height * minimum.height).toBeGreaterThanOrEqual(400);
  });
  it("hit-tests frozen geometry and rejects source slots and invalid coordinates", () => {
    const panes = dockGeometry(dockGrid(["a", "b"], 2)).panes;
    expect(dockDropTarget(panes, "a", 0.75, 0.5)).toEqual({ target: "b", zone: "center" });
    expect(dockDropTarget(panes, "a", 0.75, 0.05)).toEqual({ target: "b", zone: "top" });
    expect(dockDropTarget(panes, "a", 0.25, 0.5)).toBeNull();
    for (const x of [-0.1, 1.1, NaN, Infinity]) expect(dockDropTarget(panes, "a", x, 0.5)).toBeNull();
  });
  it("moves a complete pane below another without losing or duplicating sessions", () => {
    const initial = dockGrid(["main", "planner", "worker"], 3);
    const moved = dockMove(initial, "worker", "planner", "bottom");
    const { panes } = dockGeometry(moved);
    expect(dockIds(moved).sort()).toEqual(["main", "planner", "worker"]);
    expect(panes.main.height).toBe(1);
    expect(panes.worker.x).toBe(panes.planner.x);
    expect(panes.worker.y).toBe(panes.planner.y + panes.planner.height);
    expect(dockIds(initial)).toEqual(["main", "planner", "worker"]);
  });
  it("center drops swap pane positions while edge drops restructure the workspace", () => {
    const initial = dockGrid(["a", "b", "c", "d"], 2);
    const before = dockGeometry(initial).panes;
    const after = dockGeometry(dockMove(initial, "a", "d", "center")).panes;
    expect(after.a).toEqual(before.d);
    expect(after.d).toEqual(before.a);
    expect(after.b).toEqual(before.b);
    expect(dockMove(initial, "a", "a", "left")).toBe(initial);
    expect(dockMove(initial, "absent", "b", "top")).toBe(initial);
  });
  it("preserves a disjoint complete tiling after repeated moves in all directions", () => {
    const ids = ["a", "b", "c", "d", "e", "f"];
    const zones: DockZone[] = ["left", "top", "right", "bottom", "center"];
    let tree = dockGrid(ids, 3);
    for (let i = 0; i < 60; i++) {
      tree = dockMove(tree, ids[i % 6], ids[(i + 2) % 6], zones[i % 5]);
      expect(dockIds(tree).sort()).toEqual(ids);
      const { panes, handles } = dockGeometry(tree);
      expect(new Set(handles.map((h) => h.id)).size).toBe(5);
      const rects = Object.values(panes);
      expect(rects.reduce((area, r) => area + r.width * r.height, 0)).toBeCloseTo(1);
      for (let a = 0; a < rects.length; a++)
        for (let b = a + 1; b < rects.length; b++) {
          const x =
            Math.min(rects[a].x + rects[a].width, rects[b].x + rects[b].width) - Math.max(rects[a].x, rects[b].x);
          const y =
            Math.min(rects[a].y + rects[a].height, rects[b].y + rects[b].height) - Math.max(rects[a].y, rects[b].y);
          expect(x <= 1e-9 || y <= 1e-9).toBe(true);
        }
    }
  });
  it.each(["x", "y"])("resizes %s groups proportionally without expanding the canvas or crushing siblings", (axis) => {
    const tree = dockGrid(["a", "b", "c"], axis === "x" ? 3 : 1);
    const canvas = { width: 1200, height: 900 };
    const id = dockGeometry(tree).handles[0].id;
    const resized = dockResizeWithin(tree, id, 0.95, canvas);
    const { panes } = dockGeometry(resized);
    const dimension = axis === "x" ? "width" : "height";
    expect(panes.b[dimension]).toBeCloseTo(panes.c[dimension]);
    expect(panes.b[dimension] * canvas[dimension]).toBeCloseTo(axis === "x" ? 240 : 180);
    expect(panes.a[dimension]).toBeCloseTo(0.6);
    expect(dockMinimum(resized)[dimension]).toBeCloseTo(canvas[dimension]);
  });
  it("keeps panes readable at resize limits and matches directional previews", () => {
    const tree = dockGrid(["a", "b"], 1);
    const id = dockGeometry(tree).handles[0].id;
    const resized = dockResize(tree, id, 9);
    expect(dockGeometry(resized).panes.a.height).toBe(0.8);
    expect(dockMinimum(resized).height).toBeCloseTo(900);
    expect(dockResize(tree, id, Number.NaN)).toBe(tree);
    expect(dockZone(0.5, 0.1)).toBe("top");
    expect(dockZone(0.5, 0.5)).toBe("center");
    expect(dockZone(0.9, 0.5)).toBe("right");
    expect(dockPreview({ x: 0, y: 0, width: 1, height: 1 }, "bottom")).toEqual({ x: 0, y: 0.5, width: 1, height: 0.5 });
  });
});

it("inserts and closes nested panes without changing unaffected splits", async () => {
  const { dockInsert, dockRemove, dockIds, dockGrid, dockGeometry } = await import("./panel-dock");
  const base = dockGrid(["a", "b", "c"], 3);
  const added = dockInsert(base, "d", "b", "top");
  expect(dockIds(added)).toEqual(["a", "d", "b", "c"]);
  expect(dockGeometry(added).panes.d.height).toBe(0.5);
  expect(dockRemove(added, "d")).toEqual(base);
  expect(dockInsert(added, "d", "a", "left")).toBe(added);
  expect(dockRemove({ kind: "pane", id: "a" }, "a")).toBeNull();
});
