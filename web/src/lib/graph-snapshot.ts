import type { Edge, TaskNode } from "@/lib/types";
export type GraphSnapshot = { nodes: TaskNode[]; edges: Edge[] };

// Keep identities of unchanged records. A single worker status update should
// not invalidate every node card in the canvas.
export function shareGraphSnapshot(previous: GraphSnapshot | null, next: GraphSnapshot): GraphSnapshot {
  if (!previous) return next;
  const byId = new Map(previous.nodes.map((node) => [node.id, node]));
  const nodes = next.nodes.map((node) => {
    const old = byId.get(node.id);
    // TaskNode is flat (including the potentially large payload string). Compare
    // fields directly instead of serializing every payload twice per poll.
    const keys = Object.keys(node) as (keyof TaskNode)[];
    return old && keys.length === Object.keys(old).length && keys.every((key) => old[key] === node[key]) ? old : node;
  });
  const sameNodes = nodes.length === previous.nodes.length && nodes.every((node, i) => node === previous.nodes[i]);
  const sameEdges =
    previous.edges.length === next.edges.length &&
    next.edges.every((edge, i) => {
      const old = previous.edges[i];
      return old.src === edge.src && old.dst === edge.dst && old.rel === edge.rel;
    });
  return sameNodes && sameEdges
    ? previous
    : { nodes: sameNodes ? previous.nodes : nodes, edges: sameEdges ? previous.edges : next.edges };
}
