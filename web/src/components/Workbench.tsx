// The workbench is a Golden Layout (v2): every panel can be dragged by its tab
// to split the screen, stacked as a tab, or docked anywhere. Panels are React
// components rendered into Golden Layout's containers through portals.

import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import {
    GoldenLayout,
    Stack,
    type ComponentContainer,
    type JsonValue,
    type LayoutConfig,
    type ResolvedComponentItemConfig,
} from "golden-layout";
import { Diagram } from "./Diagram";
import { FirewallPanel } from "./FirewallPanel";
import { NamespacePanel } from "./NamespacePanel";
import { Notifier } from "./Notifier";
import { TerminalPanel } from "./TerminalPanel";

export type PanelType = "diagram" | "firewall" | "terminal" | "job" | "namespace";

export interface JobSpec {
    kind: "new" | "build" | "spawn";
    args: Record<string, unknown>;
}

interface Panel {
    key: number;
    type: PanelType;
    state: Record<string, unknown>;
}

export interface WorkbenchApi {
    /** Opens (or focuses) a terminal on a spawn's Claude Code session. */
    openTerminal(spawn: string): void;
    /** Runs a persona CLI job (new, build, spawn) in a new tab, showing its output. */
    openJob(title: string, job: JobSpec): void;
    /** Opens (or focuses) a namespace's tab. */
    openNamespace(name: string): void;
}

const WorkbenchContext = createContext<WorkbenchApi>({ openTerminal() {}, openJob() {}, openNamespace() {} });

export function useWorkbench() {
    return useContext(WorkbenchContext);
}

/** The selected namespace (null: none), shared by the diagram and namespace tabs. */
interface Selection {
    namespace: string | null;
    select(ns: string | null): void;
}

const SelectionContext = createContext<Selection>({ namespace: null, select() {} });

export function useSelection() {
    return useContext(SelectionContext);
}

const defaultLayout: LayoutConfig = {
    settings: { showPopoutIcon: false, reorderEnabled: true },
    header: { popout: false },
    dimensions: { headerHeight: 30, borderWidth: 4 },
    root: {
        type: "row",
        content: [
            {
                type: "stack",
                width: 72,
                content: [{ type: "component", componentType: "diagram", title: "Network", isClosable: false }],
            },
            {
                type: "stack",
                width: 28,
                content: [{ type: "component", componentType: "firewall", title: "Port forwards" }],
            },
        ],
    },
};

let panelSeq = 0;

export function Workbench() {
    const rootRef = useRef<HTMLDivElement>(null);
    const glRef = useRef<GoldenLayout | null>(null);
    const [panels, setPanels] = useState<Map<ComponentContainer, Panel>>(new Map());
    const [selected, setSelected] = useState<string | null>(null);
    const selection = useMemo<Selection>(() => ({ namespace: selected, select: setSelected }), [selected]);
    const panelsRef = useRef(panels);
    panelsRef.current = panels;

    useEffect(() => {
        const bind = (container: ComponentContainer, cfg: ResolvedComponentItemConfig) => {
            const panel: Panel = {
                key: ++panelSeq,
                type: cfg.componentType as PanelType,
                state: (cfg.componentState ?? {}) as Record<string, unknown>,
            };
            setPanels((p) => new Map(p).set(container, panel));
            // Not virtual: Golden Layout keeps container.element in its DOM and
            // moves it on drag; the portal inside moves with it.
            return { component: undefined, virtual: false };
        };
        const unbind = (container: ComponentContainer) => {
            setPanels((p) => {
                const n = new Map(p);
                n.delete(container);
                return n;
            });
        };
        const gl = new GoldenLayout(rootRef.current!, bind, unbind);
        gl.resizeWithContainerAutomatically = true;
        gl.loadLayout(defaultLayout);
        glRef.current = gl;
        return () => {
            glRef.current = null;
            gl.destroy();
        };
    }, []);

    // New tabs go beside the other panels, not over the diagram (the user can
    // drag them anywhere from there).
    const add = (gl: GoldenLayout, type: PanelType, state: JsonValue, title: string) => {
        for (const [container, p] of panelsRef.current) {
            const stack = container.parent.parentItem;
            if (p.type !== "diagram" && stack instanceof Stack) {
                stack.addComponent(type, state, title);
                return;
            }
        }
        gl.addComponent(type, state, title);
    };

    const api = useMemo<WorkbenchApi>(
        () => ({
            openTerminal(spawn) {
                const gl = glRef.current;
                if (!gl) return;
                for (const [container, p] of panelsRef.current) {
                    if (p.type === "terminal" && p.state.spawn === spawn) {
                        focus(container);
                        return;
                    }
                }
                add(gl, "terminal", { spawn } as JsonValue, spawn);
            },
            openJob(title, job) {
                if (glRef.current) add(glRef.current, "job", { ...job, title } as unknown as JsonValue, title);
            },
            openNamespace(name) {
                const gl = glRef.current;
                if (!gl) return;
                for (const [container, p] of panelsRef.current) {
                    if (p.type === "namespace" && p.state.name === name) {
                        focus(container);
                        return;
                    }
                }
                add(gl, "namespace", { name } as JsonValue, "ns: " + name);
            },
        }),
        [],
    );

    return (
        <WorkbenchContext.Provider value={api}>
            <SelectionContext.Provider value={selection}>
                <div ref={rootRef} className="persona-gl h-full w-full" />
                <Notifier />
                {[...panels].map(([container, p]) => createPortal(<PanelBody panel={p} container={container} />, container.element, String(p.key)))}
            </SelectionContext.Provider>
        </WorkbenchContext.Provider>
    );
}

function focus(container: ComponentContainer) {
    const item = container.parent;
    const stack = item.parentItem;
    if (stack instanceof Stack) stack.setActiveComponentItem(item, true);
    else container.focus();
}

function PanelBody({ panel, container }: { panel: Panel; container: ComponentContainer }): ReactNode {
    switch (panel.type) {
        case "diagram":
            return <Diagram />;
        case "firewall":
            return <FirewallPanel />;
        case "terminal":
            return <TerminalPanel key="t" spawn={panel.state.spawn as string} container={container} />;
        case "job":
            return <TerminalPanel key="j" job={panel.state as unknown as JobSpec & { title: string }} container={container} />;
        case "namespace":
            return <NamespacePanel name={panel.state.name as string} container={container} />;
    }
    return null;
}
