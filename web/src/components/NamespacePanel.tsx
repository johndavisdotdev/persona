// A namespace's tab: its spawns as a tree grouped by persona, and a message
// box that broadcasts to every agent in the namespace.

import { useEffect, useState } from "react";
import type { ComponentContainer } from "golden-layout";
import { ChevronDown, ChevronRight, Cpu, Network, SquareTerminal } from "lucide-react";
import { useServerState } from "@/lib/hooks";
import { isCGNAT, type Spawn } from "@/lib/types";
import { MessageBox } from "./MessageBox";
import { useToast } from "./Toasts";
import { useSelection, useWorkbench } from "./Workbench";

export function NamespacePanel({ name, container }: { name: string; container: ComponentContainer }) {
    const state = useServerState();
    const workbench = useWorkbench();
    const { namespace: selected, select } = useSelection();
    const notify = useToast();
    const [collapsed, setCollapsed] = useState<Set<string>>(new Set());

    // Showing this tab selects its namespace (in the diagram too).
    useEffect(() => {
        const onShow = () => select(name);
        container.on("show", onShow);
        container.on("focus", onShow);
        if (container.visible) select(name);
        return () => {
            container.off("show", onShow);
            container.off("focus", onShow);
        };
    }, [container, name, select]);

    const exists = state.namespaces.includes(name) || state.spawns.some((s) => s.namespace === name);
    const spawns = state.spawns.filter((s) => s.namespace === name);
    const running = spawns.filter((s) => s.state === "running");
    const groups = new Map<string, Spawn[]>();
    for (const s of spawns) groups.set(s.persona, [...(groups.get(s.persona) ?? []), s]);
    const personas = [...groups.keys()].sort();

    const toggle = (p: string) =>
        setCollapsed((c) => {
            const n = new Set(c);
            if (n.has(p)) n.delete(p);
            else n.add(p);
            return n;
        });

    return (
        <div className="flex h-full flex-col bg-[#0f1216]">
            <header className={"flex items-center gap-2 border-b px-3 py-2 " + (selected === name ? "border-violet-700" : "border-neutral-800")}>
                <Network size={16} className="text-violet-300" />
                <span className="font-mono font-semibold text-neutral-100">{name}</span>
                <span className="text-xs text-neutral-500">
                    {running.length} running{spawns.length > running.length ? ` · ${spawns.length - running.length} stopped` : ""} · {personas.length} persona
                    {personas.length === 1 ? "" : "s"}
                </span>
            </header>

            <div className="min-h-0 flex-1 overflow-auto px-2 py-2 text-sm">
                {!exists && <p className="px-2 text-neutral-500">This namespace no longer exists.</p>}
                {exists && spawns.length === 0 && <p className="px-2 text-neutral-500">No spawns here. Drag a persona onto this namespace in the Network diagram.</p>}
                <ul role="tree" className="space-y-1">
                    {personas.map((p) => {
                        const list = groups.get(p)!;
                        const open = !collapsed.has(p);
                        const up = list.filter((s) => s.state === "running").length;
                        return (
                            <li key={p} role="treeitem" aria-expanded={open}>
                                <button onClick={() => toggle(p)} className="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left hover:bg-neutral-800/60">
                                    {open ? <ChevronDown size={14} className="text-neutral-500" /> : <ChevronRight size={14} className="text-neutral-500" />}
                                    <Cpu size={14} className="text-violet-300" />
                                    <span className="font-medium text-neutral-100">{p}</span>
                                    <span className="text-xs text-neutral-500">
                                        {up}/{list.length}
                                    </span>
                                </button>
                                {open && (
                                    <ul role="group" className="mt-0.5 ml-5 space-y-0.5 border-l border-neutral-800 pl-2">
                                        {list.map((s) => (
                                            <SpawnRow
                                                key={s.name}
                                                spawn={s}
                                                ports={state.forwards.filter((f) => f.spawn === s.name).map((f) => ({ port: f.port, cg: isCGNAT(f.host_ip) }))}
                                                onOpen={() =>
                                                    s.state === "running" ? workbench.openTerminal(s.name) : notify(`${s.name} is stopped: start it from the Network diagram.`)
                                                }
                                            />
                                        ))}
                                    </ul>
                                )}
                            </li>
                        );
                    })}
                </ul>
            </div>

            <footer className="border-t border-neutral-800 p-2">
                <MessageBox namespace={name} recipients={running.length} />
            </footer>
        </div>
    );
}

function SpawnRow({ spawn: s, ports, onOpen }: { spawn: Spawn; ports: { port: number; cg: boolean }[]; onOpen(): void }) {
    const up = s.state === "running";
    return (
        <li role="treeitem">
            <button
                onClick={onOpen}
                title={s.status}
                className={"group flex w-full items-center gap-2 rounded px-1.5 py-1 text-left hover:bg-neutral-800/60 " + (up ? "" : "opacity-60")}
            >
                <span className={"inline-block h-2 w-2 flex-none rounded-full " + (up ? "bg-emerald-400" : "bg-neutral-500")} />
                <span className="text-neutral-100">{s.name}</span>
                <span className="truncate font-mono text-[11px] text-neutral-500">
                    {s.ip}
                    {s.cgnat ? ` · ${s.cgnat}` : ""}
                </span>
                {ports.map((p, i) => (
                    <span key={i} className={"rounded border px-1 font-mono text-[10px] " + (p.cg ? "border-violet-700 text-violet-300" : "border-sky-800 text-sky-300")}>
                        :{p.port}
                    </span>
                ))}
                <span className="flex-1" />
                {up && <SquareTerminal size={14} className="text-neutral-500 opacity-0 group-hover:opacity-100" />}
            </button>
        </li>
    );
}
