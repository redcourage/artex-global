import type { SessionStatus } from "./types";

export const AUTO_DONE_HOLD_MS = 1800;

export const SESSION_DRAG_TYPE = "application/x-artex-session";

export type LiveSlot = { id: string; sessionId: string | null; pinned: boolean };

export const LIVE_LAYOUTS = [
  { id: "1", columns: 1, rows: 1 },
  { id: "2", columns: 2, rows: 1 },
  { id: "3", columns: 3, rows: 1 },
  { id: "4", columns: 4, rows: 1 },
  { id: "2x2", columns: 2, rows: 2 },
  { id: "3x2", columns: 3, rows: 2 },
  { id: "1x2", columns: 1, rows: 2 },
] as const;

export type LiveLayout = (typeof LIVE_LAYOUTS)[number]["id"];

export function createLiveSlots(): LiveSlot[] {
  return Array.from({ length: 6 }, (_, i) => ({ id: `pane-${i + 1}`, sessionId: null, pinned: false }));
}

// Only fill unassigned slots. New workers never replace a session being filmed.
export function fillLiveSlots(slots: LiveSlot[], mainId: string, plannerId: string, workerIds: string[]): LiveSlot[] {
  const assigned = new Set(slots.flatMap((slot) => (slot.sessionId ? [slot.sessionId] : [])));
  const candidates = workerIds.filter((id) => !assigned.has(id));
  let changed = false;
  const next = slots.map((slot, i) => {
    if (slot.sessionId) return slot;
    const preferred = i === 0 ? mainId : i === 1 ? plannerId : undefined;
    const sessionId = preferred && !assigned.has(preferred) ? preferred : candidates.shift();
    if (!sessionId) return slot;
    changed = true;
    assigned.add(sessionId);
    return { ...slot, sessionId };
  });
  return changed ? next : slots;
}

export function canUseLiveLayout(slots: LiveSlot[], count: number): boolean {
  return !slots.slice(count).some((slot) => slot.pinned && slot.sessionId);
}

export function replaceLiveSlot(slots: LiveSlot[], index: number, sessionId: string): LiveSlot[] {
  if (!slots[index] || slots[index].pinned || slots[index].sessionId === sessionId) return slots;
  return slots.map((slot, i) => (i === index ? { ...slot, sessionId } : slot));
}

export function createAutoLiveSlots(plannerId: string): LiveSlot[] {
  const slots = createLiveSlots();
  slots[0] = { ...slots[0], sessionId: plannerId };
  return slots;
}

// Candidates arrive oldest-first. Occupied running panes never jump around;
// only Done panes can yield to another running worker. Missing/error/paused
// sessions remain visible so a transient snapshot cannot hide useful evidence.
export function advanceAutoLiveSlots(
  slots: LiveSlot[],
  workers: { id: string; status: SessionStatus }[],
  heldIds: ReadonlySet<string> = new Set(),
): LiveSlot[] {
  const shown = new Set(slots.slice(0, 3).map((slot) => slot.sessionId));
  const statuses = new Map(workers.map((worker) => [worker.id, worker.status]));
  const waiting = workers.filter((worker) => worker.status === "running" && !shown.has(worker.id));
  let changed = false;
  const next = slots.map((slot, index) => {
    if (
      index === 0 ||
      index > 2 ||
      slot.pinned ||
      (slot.sessionId && (heldIds.has(slot.sessionId) || statuses.get(slot.sessionId) !== "done"))
    )
      return slot;
    const worker = waiting.shift();
    if (!worker) return slot;
    changed = true;
    return { ...slot, sessionId: worker.id };
  });
  return changed ? next : slots;
}
