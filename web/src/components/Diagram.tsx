// The network diagram (JointJS for React): namespaces as boxes holding their
// spawns, the host along the bottom, and port forwards as links. It redraws
// from every state the server pushes over WebTransport. Personas dragged from
// the floating menu onto a namespace are spawned there; clicking a spawn
// opens its terminal.

import { createContext, useContext, useEffect, useMemo, useRef, useState, type DragEvent } from "react";
import { GraphProvider, HTMLHost, Paper } from "@joint/react";
import type { dia } from "@joint/core";
import { Box, Cpu, GripVertical, Network, Plus, Server } from "lucide-react";
import { client } from "@/lib/client";
import { useConnStatus, useServerState } from "@/lib/hooks";
import { SPAWN_NAME, defaultSpawnName, layoutNetwork, namespaceAt, nodeAt, spawnDNS, type NodeData } from "@/lib/layout";
import { isCGNAT, type Spawn } from "@/lib/types";
import { useFormDialog } from "./Dialogs";
import { useToast } from "./Toasts";
import { MessageBox } from "./MessageBox";
import { NotificationToggle } from "./Notifier";
import { useSelection, useWorkbench } from "./Workbench";

const PERSONA_MIME = "application/x-persona";

/** The namespace a persona is being dragged over, for highlighting. */
const DropTarget = createContext<string | null>(null);

interface Menu {
    x: number;
    y: number;
    items: { label: string; danger?: boolean; disabled?: boolean; run(): void }[];
}

export function Diagram() {
    const state = useServerState();
    const status = useConnStatus();
    const workbench = useWorkbench();
    const { namespace: selected, select } = useSelection();
    const openForm = useFormDialog();
    const notify = useToast();
    const paperRef = useRef<dia.Paper | null>(null);
    const wrapRef = useRef<HTMLDivElement>(null);
    const [dropNs, setDropNs] = useState<string | null>(null);
    const [menu, setMenu] = useState<Menu | null>(null);

    const layout = useMemo(() => layoutNetwork(state), [state]);

    // Fit the drawing to the panel when it changes, and when the panel resizes
    // (Golden Layout splits, docks and maximizes it).
    const fit = () => {
        const paper = paperRef.current;
        const wrap = wrapRef.current;
        if (!paper || !wrap || wrap.clientWidth === 0) return;
        paper.setDimensions(wrap.clientWidth, wrap.clientHeight);
        paper.transformToFitContent({ padding: { top: 40, bottom: 40, left: 230, right: 40 }, maxScale: 1, minScale: 0.25, useModelGeometry: true, verticalAlign: "middle", horizontalAlign: "middle" });
    };
    useEffect(() => {
        const t = setTimeout(fit, 0);
        return () => clearTimeout(t);
    }, [layout]);
    useEffect(() => {
        const ro = new ResizeObserver(() => fit());
        ro.observe(wrapRef.current!);
        return () => ro.disconnect();
    }, []);

    const toLocal = (e: DragEvent) => paperRef.current?.clientToLocalPoint({ x: e.clientX, y: e.clientY });

    const spawnPersona = (persona: string, namespace: string) => {
        openForm({
            title: `Spawn ${persona}`,
            description: `A new spawn of ${persona} in namespace ${namespace}.`,
            submit: "Spawn",
            fields: [
                {
                    name: "name",
                    label: "Spawn name",
                    initial: defaultSpawnName(persona, state.spawns.map((s) => s.name)),
                    required: true,
                    pattern: SPAWN_NAME,
                    patternHint: "1–48 lowercase letters, digits or dashes",
                },
            ],
            onSubmit: ({ name }) => {
                if (state.spawns.some((s) => s.name === name)) throw new Error(`there is already a spawn named ${name}`);
                workbench.openJob(`spawn ${name}`, { kind: "spawn", args: { persona, namespace, name } });
            },
        });
    };

    const onDragOver = (e: DragEvent) => {
        if (!e.dataTransfer.types.includes(PERSONA_MIME)) return;
        const p = toLocal(e);
        const ns = p ? namespaceAt(layout, p) : null;
        setDropNs(ns);
        if (ns) {
            e.preventDefault();
            e.dataTransfer.dropEffect = "copy";
        }
    };
    const onDrop = (e: DragEvent) => {
        const persona = e.dataTransfer.getData(PERSONA_MIME);
        const p = toLocal(e);
        const ns = p ? namespaceAt(layout, p) : null;
        setDropNs(null);
        if (!persona || !ns) return;
        e.preventDefault();
        spawnPersona(persona, ns);
    };

    const act = (op: string, args: unknown, ok: string) =>
        client.request(op, args).then(
            () => notify(ok),
            (e: Error) => notify(e.message, true),
        );

    const spawnMenu = (s: Spawn, x: number, y: number) => {
        const up = s.state === "running";
        setMenu({
            x,
            y,
            items: [
                { label: "Open terminal", disabled: !up, run: () => workbench.openTerminal(s.name) },
                up
                    ? { label: "Stop", run: () => act("spawn.stop", { spawn: s.name }, `Stopped ${s.name}`) }
                    : { label: "Start", run: () => act("spawn.start", { spawn: s.name }, `Started ${s.name}`) },
                {
                    label: "Expose a port…",
                    disabled: !up || !state.features.firewall,
                    run: () => exposePort(s.name),
                },
                {
                    label: "Message…",
                    disabled: !up,
                    run: () =>
                        openForm({
                            title: `Message ${s.name}`,
                            submit: "Send",
                            fields: [{ name: "text", label: "Message", type: "textarea", required: true }],
                            onSubmit: ({ text }) => client.request("message", { text, names: [s.name] }).then(() => notify("Sent")),
                        }),
                },
                {
                    label: "Kill",
                    danger: true,
                    run: () => {
                        if (confirm(`Kill ${s.name}? Its work is saved to its branch first.`)) void act("spawn.kill", { spawn: s.name }, `Killed ${s.name}`);
                    },
                },
            ],
        });
    };

    const exposePort = (spawn: string) =>
        openForm({
            title: `Expose a port of ${spawn}`,
            submit: "Add rule",
            fields: [
                {
                    name: "host",
                    label: "Expose on",
                    type: "select",
                    options: [
                        { value: "localhost", label: "localhost (this machine)" },
                        { value: "cgnat", label: "other spawns (100.64.x.x, every namespace)" },
                    ],
                },
                { name: "hostPort", label: "Port", type: "number", required: true, pattern: /^\d{1,5}$/ },
                { name: "port", label: `${spawn}'s port`, type: "number", required: true, pattern: /^\d{1,5}$/ },
                {
                    name: "proto",
                    label: "Protocol",
                    type: "select",
                    options: [
                        { value: "tcp", label: "TCP" },
                        { value: "udp", label: "UDP" },
                    ],
                },
            ],
            onSubmit: ({ host, hostPort, port, proto }) =>
                client.request("fw.add", { from: `${host}:${hostPort}:${proto}`, spawn, port: +port }).then(() => notify("Rule added")),
        });

    const nsMenu = (ns: string, removable: boolean, running: number, x: number, y: number) =>
        setMenu({
            x,
            y,
            items: [
                {
                    label: "Message everyone here…",
                    disabled: running === 0,
                    run: () =>
                        openForm({
                            title: `Message namespace ${ns}`,
                            submit: "Send",
                            fields: [{ name: "text", label: "Message", type: "textarea", required: true }],
                            onSubmit: ({ text }) => client.request("message", { text, namespace: ns }).then(() => notify("Sent")),
                        }),
                },
                {
                    label: "Remove namespace",
                    danger: true,
                    disabled: !removable,
                    run: () => {
                        if (confirm(`Remove namespace ${ns}?`)) void act("ns.remove", { name: ns }, `Removed namespace ${ns}`);
                    },
                },
            ],
        });

    const cellData = (model: dia.Cell) => model.get("data") as NodeData | undefined;

    // A connection drawn from a handle: spawn -> spawn in another namespace
    // (a CGNAT forward on the drop end), or between a spawn and the host (a
    // forward on the host). Same-namespace spawns already reach each other.
    const canConnect = (a: dia.Cell, b: dia.Cell) => {
        const da = cellData(a);
        const db = cellData(b);
        if (!da || !db || a.id === b.id) return false;
        if (da.kind === "host") return db.kind === "spawn" && db.spawn.state === "running";
        if (da.kind !== "spawn") return false;
        if (db.kind === "host") return true;
        return db.kind === "spawn" && db.spawn.state === "running" && db.spawn.namespace !== da.spawn.namespace;
    };

    const connect = (from: dia.Cell, to: dia.Cell) => {
        const a = cellData(from)!;
        const b = cellData(to)!;
        const portFields = [
            { name: "port", label: "Port", type: "number" as const, required: true, pattern: /^([1-9]\d{0,3}|[1-5]\d{4}|6[0-4]\d{3}|65[0-4]\d{2}|655[0-2]\d|6553[0-5])$/, patternHint: "1–65535" },
            {
                name: "proto",
                label: "Protocol",
                type: "select" as const,
                options: [
                    { value: "tcp", label: "TCP" },
                    { value: "udp", label: "UDP" },
                ],
            },
        ];
        if (a.kind === "host" || b.kind === "host") {
            const spawn = (a.kind === "spawn" ? a : (b as Extract<NodeData, { kind: "spawn" }>)).spawn;
            openForm({
                title: `Host → ${spawn.name}`,
                description: `Forward a port on the host to ${spawn.name} (${spawn.namespace}).`,
                submit: "Add rule",
                fields: [
                    ...portFields,
                    {
                        name: "listen",
                        label: "Listen address on the host",
                        type: "select",
                        options: [
                            { value: "localhost", label: "localhost (this machine only)" },
                            ...state.host_addresses.map((ip) => ({ value: ip, label: `${ip} (reachable from your network)` })),
                        ],
                    },
                ],
                onSubmit: async ({ port, proto, listen }) => {
                    await client.request("fw.add", { from: `${listen}:${port}:${proto}`, spawn: spawn.name, port: +port });
                    notify(`The host reaches ${spawn.name} at ${listen}:${port}`);
                },
            });
            return;
        }
        if (a.kind !== "spawn" || b.kind !== "spawn") return;
        const target = b.spawn; // the drop end listens
        openForm({
            title: `${a.spawn.name} → ${target.name}`,
            description: `Expose a port of ${target.name} (${target.namespace}) to spawns in other namespaces, ${a.spawn.name} (${a.spawn.namespace}) among them.`,
            submit: "Add rule",
            fields: portFields,
            onSubmit: async ({ port, proto }) => {
                await client.request("fw.add", { from: `cgnat:${port}:${proto}`, spawn: target.name, port: +port });
                notify(`${a.spawn.name} reaches ${target.name} at ${spawnDNS(target.name, target.namespace)}:${port}`);
            },
        });
    };

    return (
        <div
            ref={wrapRef}
            className="persona-diagram relative h-full w-full overflow-hidden"
            onDragOver={onDragOver}
            onDragLeave={() => setDropNs(null)}
            onDrop={onDrop}
            onClick={() => setMenu(null)}
        >
            <DropTarget.Provider value={dropNs}>
                <GraphProvider cells={layout.cells}>
                    <Paper
                        ref={paperRef}
                        style={{ width: "100%", height: "100%" }}
                        // Nothing moves; links are drawn from the handles.
                        interactive={{ elementMove: false, addLinkFromMagnet: true, linkMove: false, labelMove: false }}
                        linkPinning={false}
                        snapLinks={{ radius: 30 }}
                        defaultLink={{ type: "link", z: 4, style: { color: "#c4b5fd", width: 2, dasharray: "5 4", targetMarker: "arrow" }, connector: { name: "curve" } }}
                        // A link drawn from a handle never connects by itself
                        // (JointJS hit-tests the namespace boxes, not the HTML
                        // nodes on them): on release, find the spawn or host
                        // under the pointer from the layout, and drop the link.
                        validateConnection={() => false}
                        onLinkPointerUp={({ model, x, y, graph }) => {
                            if (String(model.id).startsWith("fw:")) return; // a rule's own link
                            // JointJS may have removed the unconnected link already
                            // (linkPinning off), so go by the ids, not the link.
                            const srcID = (model.get("source") as { id?: string } | undefined)?.id;
                            const src = srcID ? graph.getCell(srcID) : null;
                            model.remove();
                            const id = nodeAt(layout, { x, y });
                            const dst = id ? graph.getCell(id) : null;
                            if (!src || !dst) return;
                            if (canConnect(src, dst)) return connect(src, dst);
                            const a = cellData(src);
                            const b = cellData(dst);
                            if (a?.kind === "spawn" && b?.kind === "spawn" && a.spawn.namespace === b.spawn.namespace)
                                notify(`${a.spawn.name} and ${b.spawn.name} share namespace ${a.spawn.namespace}: they already reach each other (${b.spawn.name}:<port>).`, true);
                            else if (b?.kind === "spawn" && b.spawn.state !== "running") notify(`${b.spawn.name} isn't running.`, true);
                            else if (src.id !== dst.id) notify("Connect a spawn to a spawn in another namespace, or a spawn and the host.", true);
                        }}
                        drawGrid={{ name: "dot", args: { color: "#262b33" } }}
                        gridSize={20}
                        renderElement={renderNode}
                        onElementPointerClick={({ model }) => {
                            const d = cellData(model);
                            if (d?.kind === "ns") {
                                // Select the namespace and open its tab.
                                select(d.name);
                                workbench.openNamespace(d.name);
                                return;
                            }
                            if (d?.kind !== "spawn") return;
                            if (d.spawn.state === "running") workbench.openTerminal(d.spawn.name);
                            else notify(`${d.spawn.name} is stopped: right-click to start it.`);
                        }}
                        onBlankPointerClick={() => select(null)}
                        onElementContextMenu={({ model, event }) => {
                            event.preventDefault();
                            const d = cellData(model);
                            const r = wrapRef.current!.getBoundingClientRect();
                            const x = (event.clientX ?? 0) - r.left;
                            const y = (event.clientY ?? 0) - r.top;
                            if (d?.kind === "spawn") spawnMenu(d.spawn, x, y);
                            else if (d?.kind === "ns") nsMenu(d.name, d.removable, d.running, x, y);
                        }}
                    />
                </GraphProvider>
            </DropTarget.Provider>

            <PersonaMenu onNewNamespace={state.features.namespaces} />

            <div className="pointer-events-none absolute top-3 right-4 flex items-center gap-2 text-xs text-neutral-400">
                <span
                    className={
                        "inline-block h-2 w-2 rounded-full " +
                        (status.kind === "connected" ? "bg-emerald-400 shadow-[0_0_6px] shadow-emerald-400" : status.kind === "connecting" ? "bg-neutral-500" : "bg-red-500")
                    }
                />
                {status.kind === "connected" ? `connected · HTTP/3 · ${state.runtime}` : status.kind === "disconnected" ? `disconnected: ${status.detail}` : status.kind}
                <NotificationToggle />
            </div>
            {/* Message bar: the selected namespace's agents, or everyone. */}
            <div className="absolute bottom-3 left-[244px] z-10 flex w-[min(640px,calc(100%-264px))] flex-col gap-1 rounded-xl border border-neutral-700/80 bg-neutral-900/90 p-2 shadow-2xl backdrop-blur">
                {selected && (
                    <div className="flex items-center gap-2 px-1 text-xs text-neutral-400">
                        <span>
                            To namespace <span className="font-mono text-violet-300">{selected}</span>
                        </span>
                        <button onClick={() => select(null)} className="rounded px-1 text-neutral-500 hover:bg-neutral-800 hover:text-neutral-200" title="Message everyone instead">
                            ✕ everyone
                        </button>
                    </div>
                )}
                <MessageBox
                    namespace={selected}
                    recipients={state.spawns.filter((s) => s.state === "running" && (!selected || s.namespace === selected)).length}
                />
            </div>

            {state.errors?.length ? (
                <div className="absolute top-9 right-4 max-w-lg rounded-md border border-red-900 bg-red-950/80 px-3 py-1.5 text-xs text-red-200">{state.errors.join(" · ")}</div>
            ) : null}

            {menu && (
                <div className="absolute z-30 min-w-44 rounded-md border border-neutral-700 bg-neutral-900 py-1 text-sm shadow-2xl" style={{ left: menu.x, top: menu.y }}>
                    {menu.items.map((it) => (
                        <button
                            key={it.label}
                            disabled={it.disabled}
                            className={
                                "block w-full px-3 py-1.5 text-left hover:bg-neutral-800 disabled:cursor-default disabled:opacity-40 disabled:hover:bg-transparent " +
                                (it.danger ? "text-red-400" : "text-neutral-200")
                            }
                            onClick={(e) => {
                                e.stopPropagation();
                                setMenu(null);
                                it.run();
                            }}
                        >
                            {it.label}
                        </button>
                    ))}
                </div>
            )}
        </div>
    );
}

// ---- nodes ----

function renderNode(data: NodeData) {
    switch (data.kind) {
        case "ns":
            return <NamespaceNode data={data} />;
        case "spawn":
            return <SpawnNode data={data} />;
        case "host":
            return <HostNode data={data} />;
    }
}

function NamespaceNode({ data }: { data: Extract<NodeData, { kind: "ns" }> }) {
    const target = useContext(DropTarget);
    const { namespace: selected } = useSelection();
    const hot = target === data.name;
    const on = selected === data.name;
    return (
        <HTMLHost
            useModelGeometry
            className={
                "flex cursor-pointer flex-col rounded-xl border-2 p-3 transition-colors " +
                (hot
                    ? "border-dashed border-violet-400 bg-violet-500/10"
                    : on
                      ? "border-solid border-violet-500 bg-violet-500/[0.07] shadow-[0_0_24px_rgba(139,92,246,0.25)]"
                      : "border-dashed border-neutral-700 bg-neutral-900/40 hover:border-neutral-500")
            }
        >
            <div className="flex items-center gap-2 text-sm">
                <Network size={15} className="text-violet-300" />
                <span className="font-mono font-semibold text-neutral-100">{data.name}</span>
                <span className="text-xs text-neutral-500">
                    {data.running} running{data.spawns > data.running ? ` · ${data.spawns - data.running} stopped` : ""}
                </span>
            </div>
            {data.spawns === 0 && (
                <div className="flex flex-1 items-center justify-center text-xs text-neutral-500">{hot ? "drop to spawn here" : "drag a persona here"}</div>
            )}
        </HTMLHost>
    );
}

function SpawnNode({ data }: { data: Extract<NodeData, { kind: "spawn" }> }) {
    const s = data.spawn;
    const up = s.state === "running";
    return (
        <HTMLHost
            useModelGeometry
            title={s.status}
            className={
                "flex cursor-pointer flex-col gap-0.5 overflow-hidden rounded-lg border bg-neutral-800 px-3 py-2 shadow-md transition-colors hover:border-violet-400 " +
                (up ? "border-neutral-600" : "border-neutral-700 opacity-60")
            }
        >
            <div className="flex items-center gap-2">
                <span className={"inline-block h-2 w-2 flex-none rounded-full " + (up ? "bg-emerald-400" : "bg-neutral-500")} />
                <span className="truncate text-sm font-semibold text-neutral-100">{s.name}</span>
            </div>
            <div className="flex items-center gap-1 truncate text-[11px] text-neutral-400">
                <Box size={11} /> {s.persona}
            </div>
            {s.ip && (
                <div className="truncate font-mono text-[11px] text-neutral-500">
                    {s.ip}
                    {s.cgnat ? ` · ${s.cgnat}` : ""}
                </div>
            )}
            {data.ports.length > 0 && (
                <div className="flex flex-wrap gap-1">
                    {data.ports.map((f) => (
                        <span
                            key={f.id}
                            className={"rounded border px-1 font-mono text-[10px] " + (isCGNAT(f.host_ip) ? "border-violet-700 text-violet-300" : "border-sky-800 text-sky-300")}
                        >
                            :{f.port}
                        </span>
                    ))}
                </div>
            )}
        </HTMLHost>
    );
}

function HostNode({ data }: { data: Extract<NodeData, { kind: "host" }> }) {
    return (
        <HTMLHost useModelGeometry className="flex items-center gap-3 rounded-xl border border-sky-900 bg-sky-950/40 px-4">
            <Server size={20} className="text-sky-300" />
            <div className="flex flex-col">
                <span className="text-sm font-semibold text-neutral-100">Host</span>
                <span className="font-mono text-[11px] text-neutral-400">{data.exposed.length ? data.exposed.join("  ·  ") : "no ports forwarded to spawns"}</span>
            </div>
        </HTMLHost>
    );
}

// ---- floating menu ----

function PersonaMenu({ onNewNamespace }: { onNewNamespace: boolean }) {
    const state = useServerState();
    const workbench = useWorkbench();
    const openForm = useFormDialog();
    const notify = useToast();

    const newPersona = () =>
        openForm({
            title: "New persona",
            description: "Claude designs it from your description, then the image is built. The output opens in a new tab.",
            submit: "Create",
            fields: [
                { name: "name", label: "Name", required: true, pattern: SPAWN_NAME, patternHint: "lowercase letters, digits or dashes", placeholder: "go-reviewer" },
                {
                    name: "description",
                    label: "Description",
                    type: "textarea",
                    required: true,
                    placeholder: "A senior Go engineer who maintains our gRPC services and writes table-driven tests…",
                },
            ],
            onSubmit: ({ name, description }) => workbench.openJob(`new ${name}`, { kind: "new", args: { name, description } }),
        });

    const newNamespace = () =>
        openForm({
            title: "New namespace",
            description: "A separate network: its spawns reach each other, and nothing in other namespaces.",
            submit: "Create",
            fields: [{ name: "name", label: "Name", required: true, pattern: /^[a-z][a-z0-9-]{0,23}$/, patternHint: "a letter, then lowercase letters, digits or dashes (24 at most)" }],
            onSubmit: ({ name }) => client.request("ns.create", { name }).then(() => notify(`Namespace ${name} created`)),
        });

    return (
        <aside className="absolute top-3 bottom-3 left-3 z-20 flex w-52 flex-col rounded-xl border border-neutral-700/80 bg-neutral-900/90 shadow-2xl backdrop-blur">
            <div className="flex items-center gap-2 border-b border-neutral-800 px-3 py-2">
                <span className="text-violet-400">✦</span>
                <span className="text-sm font-semibold text-neutral-100">Personas</span>
            </div>
            <div className="flex-1 space-y-1.5 overflow-y-auto p-2">
                {state.personas.length === 0 && <p className="px-1 text-xs text-neutral-500">No personas yet.</p>}
                {state.personas.map((p) => (
                    <div
                        key={p.name}
                        draggable={p.built}
                        onDragStart={(e) => {
                            e.dataTransfer.setData(PERSONA_MIME, p.name);
                            e.dataTransfer.effectAllowed = "copy";
                        }}
                        title={p.built ? `${p.summary}\n\nDrag onto a namespace to spawn it.` : `${p.summary}\n\nNot built yet: build it first.`}
                        className={
                            "group flex items-center gap-1.5 rounded-md border px-2 py-1.5 text-sm " +
                            (p.built ? "cursor-grab border-neutral-700 bg-neutral-800 hover:border-violet-500 active:cursor-grabbing" : "border-dashed border-neutral-700 text-neutral-500")
                        }
                    >
                        <GripVertical size={13} className="flex-none text-neutral-500" />
                        <Cpu size={13} className="flex-none text-violet-300" />
                        <span className="flex-1 truncate">{p.name}</span>
                        {!p.built && (
                            <button
                                className="rounded px-1 text-[11px] text-violet-300 hover:bg-neutral-700"
                                onClick={() => workbench.openJob(`build ${p.name}`, { kind: "build", args: { name: p.name } })}
                            >
                                build
                            </button>
                        )}
                    </div>
                ))}
            </div>
            <div className="space-y-1.5 border-t border-neutral-800 p-2">
                <button onClick={newPersona} className="flex w-full items-center justify-center gap-1.5 rounded-md bg-violet-600 px-2 py-1.5 text-sm text-white hover:bg-violet-500">
                    <Plus size={14} /> New persona
                </button>
                {onNewNamespace && (
                    <button
                        onClick={newNamespace}
                        className="flex w-full items-center justify-center gap-1.5 rounded-md border border-neutral-700 px-2 py-1.5 text-sm text-neutral-200 hover:border-violet-500"
                    >
                        <Network size={14} /> New namespace
                    </button>
                )}
                <p className="px-1 text-[11px] leading-snug text-neutral-500">Drag a persona onto a namespace to spawn it. Click a spawn for its terminal; right-click for more.</p>
            </div>
        </aside>
    );
}
