// Lays the network out for the diagram: each namespace a box holding its
// spawns, the host a box along the bottom, and a link for every port forward
// (host -> spawn, or every other spawn -> spawn for CGNAT forwards).

import type { CellRecord } from "@joint/react";
import { forwardFrom, isCGNAT, type Forward, type Spawn, type State } from "./types";

export type NodeData =
    | { kind: "ns"; name: string; spawns: number; running: number; removable: boolean }
    | { kind: "spawn"; spawn: Spawn; ports: Forward[] }
    | { kind: "host"; exposed: string[] };

export interface Rect {
    x: number;
    y: number;
    width: number;
    height: number;
}

export interface NetworkLayout {
    cells: CellRecord<NodeData, unknown>[];
    namespaces: Map<string, Rect>;
    /** Spawn and host boxes by cell id, for finding what a link was dropped on. */
    nodes: Map<string, Rect>;
}

const TILE_W = 190;
const TILE_H = 84;
const GAP = 14;
const PAD = 18;
const HEAD = 42;
const NS_GAP = 48;
const MAX_ROW_WIDTH = 1500;
const HOST_GAP = 130;
const HOST_H = 74;

/** The connection handle: drag from one to a spawn (or the host) to forward a port. */
export const LINK_PORT = "link";
const handle = (color: string, cx: string | number, cy: string | number) => ({
    [LINK_PORT]: { cx, cy, width: 14, height: 14, shape: "ellipse", color, outline: "#0b0d10", outlineWidth: 2, className: "persona-handle" },
});

/** The name another spawn reaches spawn by, across namespaces (persona-dns). */
export const spawnDNS = (spawn: string, namespace: string) => `${spawn}.${namespace}.persona`;

export const nsID = (name: string) => "ns:" + name;
export const spawnID = (name: string) => "sp:" + name;
export const HOST_ID = "host";

export function layoutNetwork(state: State): NetworkLayout {
    const names = [...state.namespaces];
    for (const s of state.spawns) if (!names.includes(s.namespace)) names.push(s.namespace);

    const cells: CellRecord<NodeData, unknown>[] = [];
    const rects = new Map<string, Rect>();
    const centers = new Map<string, { x: number; y: number }>();
    const nodes = new Map<string, Rect>();
    let x = 0;
    let y = 0;
    let rowH = 0;
    let maxRight = 0;

    for (const ns of names) {
        const spawns = state.spawns.filter((s) => s.namespace === ns);
        const cols = Math.max(2, Math.min(3, spawns.length));
        const rows = Math.max(1, Math.ceil(spawns.length / cols));
        const w = PAD * 2 + cols * TILE_W + (cols - 1) * GAP;
        const h = HEAD + PAD + rows * TILE_H + (rows - 1) * GAP;
        if (x > 0 && x + w > MAX_ROW_WIDTH) {
            x = 0;
            y += rowH + NS_GAP;
            rowH = 0;
        }
        const rect = { x, y, width: w, height: h };
        rects.set(ns, rect);
        cells.push({
            id: nsID(ns),
            type: "element",
            position: { x, y },
            size: { width: w, height: h },
            z: 1,
            data: {
                kind: "ns",
                name: ns,
                spawns: spawns.length,
                running: spawns.filter((s) => s.state === "running").length,
                removable: ns !== "default" && spawns.length === 0 && state.features.namespaces,
            },
        });
        spawns.forEach((s, i) => {
            const pos = { x: x + PAD + (i % cols) * (TILE_W + GAP), y: y + HEAD + Math.floor(i / cols) * (TILE_H + GAP) };
            centers.set(s.name, { x: pos.x + TILE_W / 2, y: pos.y + TILE_H / 2 });
            nodes.set(spawnID(s.name), { ...pos, width: TILE_W, height: TILE_H });
            cells.push({
                id: spawnID(s.name),
                type: "element",
                position: pos,
                size: { width: TILE_W, height: TILE_H },
                z: 2,
                portMap: s.state === "running" ? handle("#8b5cf6", "100%", "50%") : undefined,
                data: { kind: "spawn", spawn: s, ports: state.forwards.filter((f) => f.spawn === s.name) },
            });
        });
        x += w + NS_GAP;
        rowH = Math.max(rowH, h);
        maxRight = Math.max(maxRight, x - NS_GAP);
    }

    const hostForwards = state.forwards.filter((f) => !isCGNAT(f.host_ip));
    const hostW = Math.max(maxRight, 400);
    nodes.set(HOST_ID, { x: 0, y: y + rowH + HOST_GAP, width: hostW, height: HOST_H });
    cells.push({
        id: HOST_ID,
        type: "element",
        position: { x: 0, y: y + rowH + HOST_GAP },
        size: { width: hostW, height: HOST_H },
        z: 1,
        portMap: handle("#38bdf8", "50%", 0),
        data: { kind: "host", exposed: [...new Set(hostForwards.map(forwardFrom))] },
    });

    // Links: traffic flows from the source (host, or another spawn) to the spawn.
    const known = new Set(state.spawns.map((s) => s.name));
    const running = state.spawns.filter((s) => s.state === "running");
    const nsOf = new Map(state.spawns.map((s) => [s.name, s.namespace]));
    for (const f of state.forwards) {
        if (!known.has(f.spawn)) continue;
        if (isCGNAT(f.host_ip)) {
            // Every spawn in another namespace can reach it (those in its own
            // already do, directly): one link from each, arcing over the
            // boxes, each a little higher so they don't overlap. The label is
            // the name they reach it by.
            const ns = nsOf.get(f.spawn)!;
            const sources = running.filter((o) => o.namespace !== ns);
            sources.forEach((other, i) => {
                cells.push({
                    id: `fw:${f.id}:${other.name}`,
                    type: "link",
                    source: { id: spawnID(other.name), anchor: { name: "top" } },
                    target: { id: spawnID(f.spawn), anchor: { name: "top" } },
                    z: 3,
                    style: { color: "#a78bfa", width: 1.5, dasharray: "6 4", targetMarker: "arrow" },
                    labelMap:
                        i === 0
                            ? { l: { text: `${spawnDNS(f.spawn, ns)}:${f.host_port}${f.proto === "udp" ? "/udp" : ""}`, fontSize: 11, color: "#ddd6fe", backgroundColor: "#1e1b2e" } }
                            : {},
                    connector: {
                        name: "curve",
                        args: { sourceDirection: "up", targetDirection: "up", distanceCoefficient: 0.45 + 0.2 * i },
                    },
                } as CellRecord<NodeData, unknown>);
            });
        } else {
            // From the host's top edge, part way toward the spawn (so links
            // fan out), curving up into the spawn's bottom.
            const c = centers.get(f.spawn);
            const dx = c ? (c.x - hostW / 2) * 0.6 : 0;
            cells.push({
                id: `fw:${f.id}`,
                type: "link",
                source: { id: HOST_ID, anchor: { name: "top", args: { dx } } },
                target: { id: spawnID(f.spawn), anchor: { name: "bottom" } },
                z: 3,
                style: { color: "#38bdf8", width: 2, targetMarker: "arrow" },
                // The address the host reaches it on.
                labelMap: { l: { text: forwardFrom(f), fontSize: 11, color: "#e0f2fe", backgroundColor: "#0c1d2b" } },
                connector: { name: "curve", args: { sourceDirection: "up", targetDirection: "down" } },
            } as CellRecord<NodeData, unknown>);
        }
    }
    return { cells, namespaces: rects, nodes };
}

/** The spawn or host box (cell id) containing the point, if any. */
export function nodeAt(layout: NetworkLayout, p: { x: number; y: number }): string | null {
    for (const [id, r] of layout.nodes) {
        if (p.x >= r.x && p.x <= r.x + r.width && p.y >= r.y && p.y <= r.y + r.height) return id;
    }
    return null;
}

/** The namespace whose box contains the point (in diagram coordinates). */
export function namespaceAt(layout: NetworkLayout, p: { x: number; y: number }): string | null {
    for (const [name, r] of layout.namespaces) {
        if (p.x >= r.x && p.x <= r.x + r.width && p.y >= r.y && p.y <= r.y + r.height) return name;
    }
    return null;
}

/** A valid, unused spawn name for persona: <persona>-<n>. */
export function defaultSpawnName(persona: string, taken: Iterable<string>): string {
    const used = new Set(taken);
    for (let n = 1; ; n++) {
        const suffix = "-" + n;
        const name = persona.slice(0, 48 - suffix.length) + suffix;
        if (!used.has(name)) return name;
    }
}

export const SPAWN_NAME = /^[a-z0-9][a-z0-9-]{0,47}$/;
