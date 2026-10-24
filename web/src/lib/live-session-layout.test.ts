import { describe, expect, it } from "vitest";

import {
  advanceAutoLiveSlots,
  createAutoLiveSlots,
  canUseLiveLayout,
  createLiveSlots,
  fillLiveSlots,
  replaceLiveSlot,
} from "./live-session-layout";

describe("live recording layout", () => {
  it("assigns the current main, planner and newest workers once; later workers never steal an occupied pane", () => {
    const slots = fillLiveSlots(createLiveSlots(), "main:3", "plan", ["14", "13"]);
    expect(slots.every((slot) => !slot.pinned)).toBe(true);
    expect(slots.map((slot) => slot.sessionId)).toEqual(["main:3", "plan", "14", "13", null, null]);
    const later = fillLiveSlots(slots, "main:4", "plan", ["16", "15", "14", "13"]);
    expect(later.map((slot) => slot.sessionId)).toEqual(["main:3", "plan", "14", "13", "16", "15"]);
    expect(fillLiveSlots(later, "main:5", "plan", ["17"])).toBe(later);
  });
  it("protects pinned panes from selection and layout changes that would hide them", () => {
    const slots = fillLiveSlots(createLiveSlots(), "main", "plan", ["14"]);
    slots[0] = { ...slots[0], pinned: true };
    slots[1] = { ...slots[1], pinned: true };
    expect(replaceLiveSlot(slots, 0, "14")).toBe(slots);
    expect(replaceLiveSlot(slots, 2, "13")[2].sessionId).toBe("13");
    expect(canUseLiveLayout(slots, 1)).toBe(false);
    expect(canUseLiveLayout(slots, 2)).toBe(true);
    slots[5] = { ...slots[5], pinned: true, sessionId: "16" };
    expect(canUseLiveLayout(slots, 4)).toBe(false);
    expect(canUseLiveLayout(slots, 6)).toBe(true);
  });
});

describe("automatic Planner and two-worker layout", () => {
  it("starts with Planner and adds only running workers up to two without moving occupied panes", () => {
    const slots = createAutoLiveSlots("plan");
    expect(slots.filter((slot) => slot.sessionId).map((slot) => slot.sessionId)).toEqual(["plan"]);
    expect(advanceAutoLiveSlots(slots, [{ id: "1", status: "pending" }])).toBe(slots);
    const one = advanceAutoLiveSlots(slots, [{ id: "1", status: "running" }]);
    expect(one.slice(0, 3).map((slot) => slot.sessionId)).toEqual(["plan", "1", null]);
    const two = advanceAutoLiveSlots(one, [
      { id: "1", status: "running" },
      { id: "2", status: "running" },
      { id: "3", status: "running" },
    ]);
    expect(two.slice(0, 3).map((slot) => slot.sessionId)).toEqual(["plan", "1", "2"]);
    expect(
      advanceAutoLiveSlots(two, [
        { id: "3", status: "running" },
        { id: "2", status: "running" },
        { id: "1", status: "running" },
      ]),
    ).toBe(two);
  });
  it("replaces Done with an unshown running worker, retaining Done when no replacement exists", () => {
    const slots = advanceAutoLiveSlots(createAutoLiveSlots("plan"), [
      { id: "1", status: "running" },
      { id: "2", status: "running" },
    ]);
    const next = advanceAutoLiveSlots(slots, [
      { id: "1", status: "done" },
      { id: "2", status: "running" },
      { id: "3", status: "pending" },
      { id: "4", status: "running" },
    ]);
    expect(next.slice(0, 3).map((slot) => slot.sessionId)).toEqual(["plan", "4", "2"]);
    expect(next[1].id).toBe(slots[1].id);
    expect(
      advanceAutoLiveSlots(next, [
        { id: "4", status: "done" },
        { id: "2", status: "done" },
      ]),
    ).toBe(next);
  });
  it("does not replace missing, paused, failed or pinned sessions", () => {
    for (const status of ["paused", "blocked", "exhausted", "stopped"] as const) {
      const slots = advanceAutoLiveSlots(createAutoLiveSlots("plan"), [
        { id: "1", status: "running" },
        { id: "2", status: "running" },
      ]);
      expect(
        advanceAutoLiveSlots(slots, [
          { id: "1", status },
          { id: "3", status: "running" },
        ]),
      ).toBe(slots);
      slots[1].pinned = true;
      expect(
        advanceAutoLiveSlots(slots, [
          { id: "1", status: "done" },
          { id: "3", status: "running" },
        ]),
      ).toBe(slots);
    }
  });
});

it("keeps a Done pane during its completion glow, then releases it to a running worker", () => {
  const slots = advanceAutoLiveSlots(createAutoLiveSlots("plan"), [
    { id: "1", status: "running" },
    { id: "2", status: "running" },
  ]);
  const workers = [
    { id: "1", status: "done" as const },
    { id: "2", status: "running" as const },
    { id: "3", status: "running" as const },
  ];
  expect(advanceAutoLiveSlots(slots, workers, new Set(["1"]))).toBe(slots);
  expect(
    advanceAutoLiveSlots(slots, workers)
      .slice(0, 3)
      .map((slot) => slot.sessionId),
  ).toEqual(["plan", "3", "2"]);
});
