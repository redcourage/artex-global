export type DockZone = "left" | "right" | "top" | "bottom" | "center";
export type DockNode =
  | { kind: "pane"; id: string }
  | { kind: "split"; id: string; axis: "x" | "y"; ratio: number; first: DockNode; second: DockNode };
export type DockRect = { x: number; y: number; width: number; height: number };

export function dockIds(node: DockNode): string[] {
  return node.kind === "pane" ? [node.id] : [...dockIds(node.first), ...dockIds(node.second)];
}

export function dockGrid(ids: string[], columns: number): DockNode {
  if (!ids.length) throw new Error("A workspace needs at least one pane");
  const join = (nodes: DockNode[], axis: "x" | "y", prefix: string): DockNode =>
    nodes.length === 1
      ? nodes[0]
      : {
          kind: "split",
          id: `${prefix}-${nodes.length}`,
          axis,
          ratio: 1 / nodes.length,
          first: nodes[0],
          second: join(nodes.slice(1), axis, prefix),
        };
  const rows: DockNode[] = [];
  for (let i = 0; i < ids.length; i += columns) {
    rows.push(
      join(
        ids.slice(i, i + columns).map((id) => ({ kind: "pane", id })),
        "x",
        `row-${i}`,
      ),
    );
  }
  return join(rows, "y", "rows");
}

export function dockRemove(node: DockNode, id: string): DockNode | null {
  if (node.kind === "pane") return node.id === id ? null : node;
  const first = dockRemove(node.first, id),
    second = dockRemove(node.second, id);
  return !first ? second : !second ? first : { ...node, first, second };
}

export function dockMove(tree: DockNode, source: string, target: string, zone: DockZone): DockNode {
  const ids = dockIds(tree);
  if (source === target || !ids.includes(source) || !ids.includes(target)) return tree;
  if (zone === "center") {
    const swap = (n: DockNode): DockNode =>
      n.kind === "pane"
        ? { ...n, id: n.id === source ? target : n.id === target ? source : n.id }
        : { ...n, first: swap(n.first), second: swap(n.second) };
    return swap(tree);
  }
  return dockInsert(dockRemove(tree, source)!, source, target, zone);
}

export function dockInsert(tree: DockNode, source: string, target: string, requestedZone: DockZone): DockNode {
  const zone = requestedZone === "center" ? "right" : requestedZone;
  const ids = dockIds(tree);
  if (ids.includes(source) || !ids.includes(target)) return tree;
  const remaining = tree;
  const splitIds = new Set<string>();
  const collect = (n: DockNode) => {
    if (n.kind === "split") {
      splitIds.add(n.id);
      collect(n.first);
      collect(n.second);
    }
  };
  collect(remaining);
  let sequence = 0;
  while (splitIds.has(`dock-${sequence}`)) sequence++;
  const insert = (n: DockNode): DockNode => {
    if (n.kind === "split") return { ...n, first: insert(n.first), second: insert(n.second) };
    if (n.id !== target) return n;
    const moving: DockNode = { kind: "pane", id: source };
    const before = zone === "left" || zone === "top";
    return {
      kind: "split",
      id: `dock-${sequence}`,
      axis: zone === "left" || zone === "right" ? "x" : "y",
      ratio: 0.5,
      first: before ? moving : n,
      second: before ? n : moving,
    };
  };
  return insert(remaining);
}

export function dockResize(tree: DockNode, id: string, ratio: number): DockNode {
  if (tree.kind === "pane" || !Number.isFinite(ratio)) return tree;
  if (tree.id === id) return { ...tree, ratio: Math.min(0.8, Math.max(0.2, ratio)) };
  return { ...tree, first: dockResize(tree.first, id, ratio), second: dockResize(tree.second, id, ratio) };
}

// Preserve each sibling subtree's proportions while keeping its panes readable.
// Constrain the split to the current canvas rather than growing the canvas during a gesture.
export function dockResizeWithin(
  tree: DockNode,
  id: string,
  ratio: number,
  canvas: { width: number; height: number },
  sizes: Record<string, { width: number; height: number } | undefined> = {},
): DockNode {
  if (tree.kind === "pane" || !Number.isFinite(ratio)) return tree;
  if (tree.id === id) {
    const extent = tree.axis === "x" ? canvas.width : canvas.height;
    if (!(extent > 0)) return dockResize(tree, id, ratio);
    const dimension = tree.axis === "x" ? "width" : "height";
    const lower = Math.max(0.2, dockMinimum(tree.first, sizes)[dimension] / extent);
    const upper = Math.min(0.8, 1 - dockMinimum(tree.second, sizes)[dimension] / extent);
    if (lower > upper) return tree;
    return dockResize(tree, id, Math.max(lower, Math.min(upper, ratio)));
  }
  const firstSize =
    tree.axis === "x"
      ? { ...canvas, width: canvas.width * tree.ratio }
      : { ...canvas, height: canvas.height * tree.ratio };
  const secondSize =
    tree.axis === "x"
      ? { ...canvas, width: canvas.width * (1 - tree.ratio) }
      : { ...canvas, height: canvas.height * (1 - tree.ratio) };
  return {
    ...tree,
    first: dockResizeWithin(tree.first, id, ratio, firstSize, sizes),
    second: dockResizeWithin(tree.second, id, ratio, secondSize, sizes),
  };
}

export function dockGeometry(tree: DockNode) {
  const panes: Record<string, DockRect> = {};
  const handles: { id: string; axis: "x" | "y"; ratio: number; rect: DockRect }[] = [];
  const walk = (n: DockNode, r: DockRect) => {
    if (n.kind === "pane") {
      panes[n.id] = r;
      return;
    }
    handles.push({ id: n.id, axis: n.axis, ratio: n.ratio, rect: r });
    if (n.axis === "x") {
      walk(n.first, { ...r, width: r.width * n.ratio });
      walk(n.second, { ...r, x: r.x + r.width * n.ratio, width: r.width * (1 - n.ratio) });
    } else {
      walk(n.first, { ...r, height: r.height * n.ratio });
      walk(n.second, { ...r, y: r.y + r.height * n.ratio, height: r.height * (1 - n.ratio) });
    }
  };
  walk(tree, { x: 0, y: 0, width: 1, height: 1 });
  return { panes, handles };
}

export function dockMinimum(
  tree: DockNode,
  sizes: Record<string, { width: number; height: number } | undefined> = {},
): { width: number; height: number } {
  if (tree.kind === "pane") {
    const size = sizes[tree.id];
    return {
      width: size && Number.isFinite(size.width) && size.width > 0 ? size.width : 240,
      height: size && Number.isFinite(size.height) && size.height > 0 ? size.height : 180,
    };
  }
  const a = dockMinimum(tree.first, sizes),
    b = dockMinimum(tree.second, sizes);
  return tree.axis === "x"
    ? { width: Math.max(a.width / tree.ratio, b.width / (1 - tree.ratio)), height: Math.max(a.height, b.height) }
    : { width: Math.max(a.width, b.width), height: Math.max(a.height / tree.ratio, b.height / (1 - tree.ratio)) };
}

// A gesture keeps these rectangles frozen while the live preview animates. Using
// elementFromPoint on the animated panes would make a stationary pointer switch
// targets as its destination moved away, causing flicker or repeated swaps.
export function dockDropTarget(
  panes: Record<string, DockRect>,
  source: string,
  x: number,
  y: number,
): { target: string; zone: DockZone } | null {
  if (!Number.isFinite(x) || !Number.isFinite(y) || x < 0 || x > 1 || y < 0 || y > 1) return null;
  for (const [id, r] of Object.entries(panes)) {
    if (id !== source && x >= r.x && x <= r.x + r.width && y >= r.y && y <= r.y + r.height) {
      return { target: id, zone: dockZone((x - r.x) / r.width, (y - r.y) / r.height) };
    }
  }
  return null;
}

// Normalized pointer position inside a target pane. The middle swaps positions.
export function dockZone(x: number, y: number): DockZone {
  if (x >= 0.25 && x <= 0.75 && y >= 0.25 && y <= 0.75) return "center";
  const distances = [x, 1 - x, y, 1 - y];
  return (["left", "right", "top", "bottom"] as const)[distances.indexOf(Math.min(...distances))];
}

export function dockPreview(rect: DockRect, zone: DockZone): DockRect {
  if (zone === "center") return rect;
  if (zone === "left" || zone === "right")
    return { ...rect, x: rect.x + (zone === "right" ? rect.width / 2 : 0), width: rect.width / 2 };
  return { ...rect, y: rect.y + (zone === "bottom" ? rect.height / 2 : 0), height: rect.height / 2 };
}
