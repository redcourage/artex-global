import { expect, it } from "vitest";
import type { TaskNode } from "./types";
import { shareGraphSnapshot } from "./graph-snapshot";
it("preserves 999 node identities when one worker changes in a 1000-node snapshot", () => {
  const old = {
    nodes: Array.from({ length: 1000 }, (_, i) => ({ id: String(i), type: "intent", state: "running" }) as TaskNode),
    edges: [{ src: "0", dst: "1", rel: "spawns" as const }],
  };
  const nodes = old.nodes.map((node) => ({ ...node }));
  nodes[501].state = "done";
  const next = shareGraphSnapshot(old, { nodes, edges: old.edges.map((edge) => ({ ...edge })) });
  expect(next.nodes.filter((node, i) => node === old.nodes[i])).toHaveLength(999);
  expect(next.edges).toBe(old.edges);
  expect(next.nodes[501].state).toBe("done");
  expect(old.nodes[501].state).toBe("running");
  expect(shareGraphSnapshot(next, { nodes: nodes.map((node) => ({ ...node })), edges: [...old.edges] })).toBe(next);
});
it("keeps unchanged nodes across removals, additions and reordering", () => {
  const old = { nodes: ["a", "b"].map((id) => ({ id, type: "intent" }) as TaskNode), edges: [] };
  const next = shareGraphSnapshot(old, {
    nodes: [{ id: "c", type: "intent" } as TaskNode, { ...old.nodes[1] }],
    edges: [],
  });
  expect(next.nodes.map((n) => n.id)).toEqual(["c", "b"]);
  expect(next.nodes[1]).toBe(old.nodes[1]);
});
