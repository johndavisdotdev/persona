// The state `persona server` pushes on its control stream (internal/server).

export interface Spawn {
    name: string;
    container: string;
    persona: string;
    namespace: string;
    state: string; // "running" | "exited" | ...
    status: string;
    ip?: string;
    cgnat?: string;
    created: string;
}

export interface PersonaInfo {
    name: string;
    summary: string;
    harness: string;
    built: boolean;
}

export interface Forward {
    id: number;
    spawn: string;
    namespace: string;
    proto: "tcp" | "udp";
    host_ip: string;
    host_port: number;
    port: number;
}

export interface State {
    runtime: string;
    namespaces: string[];
    spawns: Spawn[];
    personas: PersonaInfo[];
    forwards: Forward[];
    features: { namespaces: boolean; firewall: boolean };
    /** The host's own IPv4 addresses a port forward can listen on (besides localhost). */
    host_addresses: string[];
    errors?: string[];
}

/** An agent reporting to the host through its beacon (report_work_finished, request_input). */
export interface Notice {
    spawn: string;
    persona: string;
    namespace: string;
    kind: "finished" | "input";
    question?: string;
    options?: string[];
    ts: string;
}

export const emptyState: State = {
    runtime: "",
    namespaces: [],
    spawns: [],
    personas: [],
    forwards: [],
    features: { namespaces: false, firewall: false },
    host_addresses: [],
};

export type ConnStatus =
    | { kind: "connecting"; detail?: string }
    | { kind: "connected" }
    | { kind: "disconnected"; detail: string }
    | { kind: "unsupported" }
    | { kind: "needs-token" };

/** Whether ip is in 100.64.0.0/10, where spawns expose ports to other spawns. */
export function isCGNAT(ip: string): boolean {
    const m = /^100\.(\d+)\./.exec(ip);
    return !!m && +m[1] >= 64 && +m[1] <= 127;
}

export function forwardFrom(f: Forward): string {
    const host = f.host_ip === "127.0.0.1" ? "localhost" : f.host_ip;
    return `${host}:${f.host_port}${f.proto === "udp" ? "/udp" : ""}`;
}
