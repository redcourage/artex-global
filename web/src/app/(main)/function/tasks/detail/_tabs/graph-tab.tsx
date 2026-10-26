"use client";

import * as React from "react";

import { ExplorationGraph } from "@/components/exploration-graph";
import { Card, CardContent } from "@/components/ui/card";
import { api } from "@/lib/api";
import { shareGraphSnapshot } from "@/lib/graph-snapshot";
import type { Edge, TaskNode } from "@/lib/types";

export function GraphTab({ taskId }: { taskId: string }) {
  const [nodes, setNodes] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  // 이전 그래프 데이터 서명. 같은 폴링 데이터면 setState를 건너뛰어 불필요한 전체 재구성과
  // 드래그 중 20초 폴링에 따른 끊김을 방지한다. 렌더링에 영향을 주는 필드만 사용한다.
  const sigRef = React.useRef("");

  React.useEffect(() => {
    let cancelled = false;
    sigRef.current = ""; // 작업 변경: 다음 갱신 강제.
    const load = () => {
      api
        .explorationGraph(taskId)
        .then((g) => {
          if (cancelled) return;
          const ns = g.nodes ?? [];
          const es = g.edges ?? [];
          const sig = JSON.stringify([
            ns.map((n) => [n.id, n.type, n.state, n.priority, n.payload]),
            es.map((e) => [e.src, e.dst, e.rel]),
          ]);
          if (sig === sigRef.current) return; // 변화 없음 → 재구성 생략.
          sigRef.current = sig;
          setNodes((previous) => shareGraphSnapshot({ nodes: previous, edges: [] }, { nodes: ns, edges: [] }).nodes);
          setEdges(es);
        })
        .catch(() => {
          /* keep last good data */
        });
    };
    load();
    const timer = setInterval(load, 20000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId]);

  return (
    <Card>
      <CardContent className="p-0">
        <ExplorationGraph nodes={nodes} edges={edges} className="h-[72vh]" />
      </CardContent>
    </Card>
  );
}
