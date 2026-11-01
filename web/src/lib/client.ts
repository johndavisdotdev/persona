// Client for `persona server` over WebTransport (see internal/server and the
// README's "Web UI" section).
//
//   1. GET /config.json -> { url, certHash? }
//   2. new WebTransport(url + "?token=...", { serverCertificateHashes })
//   3. Each bidirectional stream starts with one JSON header line:
//      - {"type":"control"}: newline-delimited JSON requests/responses, plus
//        {"event":"state","data":State} pushed on every change.
//      - {"type":"attach",...} / {"type":"job",...}: frames of
//        [type u8][len u32 BE][payload]; 0 data, 1 resize, 2 exit.

import { emptyState, type ConnStatus, type Notice, type State } from "./types";

const enc = new TextEncoder();
const dec = new TextDecoder();

const TOKEN_KEY = "persona-token";

/** Where the server is: same origin, or VITE_PERSONA_SERVER for `vite dev`. */
const serverBase = (import.meta.env?.VITE_PERSONA_SERVER as string | undefined)?.replace(/\/$/, "") ?? "";

type Listener = () => void;

interface Pending {
    resolve: (v: unknown) => void;
    reject: (e: Error) => void;
}

export interface StreamHandle {
    /** Send keystrokes (attach streams). */
    write(data: string): void;
    resize(cols: number, rows: number): void;
    close(): void;
}

export interface StreamCallbacks {
    onData(bytes: Uint8Array): void;
    onExit(code: number, error: string): void;
}

export class PersonaClient {
    private wt: WebTransport | null = null;
    private writer: WritableStreamDefaultWriter<Uint8Array> | null = null;
    private nextID = 1;
    private pending = new Map<number, Pending>();
    private listeners = new Set<Listener>();
    private noticeListeners = new Set<(n: Notice) => void>();
    private tokenWaiters: ((t: string) => void)[] = [];
    private started = false;

    state: State = emptyState;
    status: ConnStatus = { kind: "connecting" };

    subscribe = (l: Listener) => {
        this.listeners.add(l);
        return () => this.listeners.delete(l);
    };
    getState = () => this.state;
    /** Agents' reports (work finished, input needed) as they arrive. */
    onNotice = (l: (n: Notice) => void) => {
        this.noticeListeners.add(l);
        return () => {
            this.noticeListeners.delete(l);
        };
    };
    getStatus = () => this.status;

    private emit() {
        for (const l of this.listeners) l();
    }

    private setStatus(s: ConnStatus) {
        this.status = s;
        this.emit();
    }

    /** Supplies the access token when the status is "needs-token". */
    provideToken(token: string) {
        try {
            localStorage.setItem(TOKEN_KEY, token);
        } catch {
            /* private mode */
        }
        const waiters = this.tokenWaiters;
        this.tokenWaiters = [];
        for (const w of waiters) w(token);
    }

    private takeToken(): string | null {
        const m = location.hash.match(/token=([0-9a-f]+)/);
        if (m) {
            try {
                localStorage.setItem(TOKEN_KEY, m[1]);
            } catch {
                /* private mode */
            }
            history.replaceState(null, "", location.pathname + location.search);
            return m[1];
        }
        try {
            return localStorage.getItem(TOKEN_KEY);
        } catch {
            return null;
        }
    }

    private askToken(): Promise<string> {
        this.setStatus({ kind: "needs-token" });
        return new Promise((resolve) => this.tokenWaiters.push(resolve));
    }

    /** Connects, and reconnects for as long as the page lives. */
    start() {
        if (this.started) return;
        this.started = true;
        void this.run();
    }

    private async run() {
        if (!("WebTransport" in window)) {
            this.setStatus({ kind: "unsupported" });
            return;
        }
        let token = this.takeToken() ?? (await this.askToken());
        let delay = 1000;
        for (;;) {
            try {
                this.setStatus({ kind: "connecting" });
                const cfg: { url: string; certHash?: string } = await (
                    await fetch(serverBase + "/config.json", { cache: "no-store" })
                ).json();
                const opts: WebTransportOptions = {};
                if (cfg.certHash) {
                    const hash = Uint8Array.from(atob(cfg.certHash), (c) => c.charCodeAt(0));
                    opts.serverCertificateHashes = [{ algorithm: "sha-256", value: hash }];
                }
                this.wt = new WebTransport(cfg.url + "?token=" + encodeURIComponent(token), opts);
                await this.wt.ready;
                await this.openControl();
                this.setStatus({ kind: "connected" });
                delay = 1000;
                const why = await this.wt.closed.then(
                    () => "closed",
                    (e: Error) => e?.message || "closed",
                );
                this.setStatus({ kind: "disconnected", detail: why });
            } catch (e) {
                const msg = String((e as Error)?.message ?? e);
                if (/401|unauthori[sz]ed|token/i.test(msg)) {
                    token = await this.askToken();
                    continue;
                }
                this.setStatus({ kind: "disconnected", detail: msg });
            }
            for (const [, p] of this.pending) p.reject(new Error("disconnected"));
            this.pending.clear();
            this.writer = null;
            this.wt = null;
            await new Promise((r) => setTimeout(r, delay));
            delay = Math.min(delay * 2, 15000);
        }
    }

    private async openControl() {
        const s = await this.wt!.createBidirectionalStream();
        const writer = s.writable.getWriter();
        await writer.write(enc.encode(JSON.stringify({ type: "control" }) + "\n"));
        this.writer = writer;
        void (async () => {
            const reader = s.readable.pipeThrough(new TextDecoderStream()).getReader();
            let buf = "";
            for (;;) {
                const { value, done } = await reader.read();
                if (done) break;
                buf += value;
                let i: number;
                while ((i = buf.indexOf("\n")) >= 0) {
                    const line = buf.slice(0, i);
                    buf = buf.slice(i + 1);
                    if (line) this.onControl(JSON.parse(line));
                }
            }
        })().catch(() => {});
    }

    private onControl(msg: { event?: string; data?: unknown; id?: number; ok?: boolean; result?: unknown; error?: string }) {
        if (msg.event === "notice" && msg.data) {
            for (const l of this.noticeListeners) l(msg.data as Notice);
            return;
        }
        if (msg.event === "state" && msg.data) {
            // Go sends empty lists as null.
            const d = msg.data as State;
            this.state = {
                ...emptyState,
                ...d,
                namespaces: d.namespaces ?? [],
                spawns: d.spawns ?? [],
                personas: d.personas ?? [],
                forwards: d.forwards ?? [],
                features: d.features ?? emptyState.features,
                host_addresses: d.host_addresses ?? [],
            };
            this.emit();
            return;
        }
        const p = msg.id !== undefined ? this.pending.get(msg.id) : undefined;
        if (!p) return;
        this.pending.delete(msg.id!);
        if (msg.ok) p.resolve(msg.result);
        else p.reject(new Error(msg.error || "failed"));
    }

    /** A control request: ns.create, fw.add, spawn.kill, message, ... */
    request<T = unknown>(op: string, args?: unknown): Promise<T> {
        if (!this.writer) return Promise.reject(new Error("not connected"));
        const id = this.nextID++;
        return new Promise<T>((resolve, reject) => {
            this.pending.set(id, { resolve: resolve as (v: unknown) => void, reject });
            this.writer!.write(enc.encode(JSON.stringify({ id, op, args }) + "\n")).catch(reject);
        });
    }

    /**
     * Opens a terminal (type "attach") or job (type "job") stream. Output and
     * the exit status arrive through cb; the handle sends input and resizes.
     */
    async openStream(header: Record<string, unknown>, cols: number, rows: number, cb: StreamCallbacks): Promise<StreamHandle> {
        if (!this.wt) throw new Error("not connected");
        const s = await this.wt.createBidirectionalStream();
        const writer = s.writable.getWriter();
        let closed = false;
        const frame = (type: number, payload: Uint8Array) => {
            if (closed) return;
            const b = new Uint8Array(5 + payload.length);
            b[0] = type;
            new DataView(b.buffer).setUint32(1, payload.length);
            b.set(payload, 5);
            writer.write(b).catch(() => {});
        };
        await writer.write(enc.encode(JSON.stringify({ ...header, cols, rows }) + "\n"));

        void (async () => {
            const reader = s.readable.getReader();
            let buf = new Uint8Array(0);
            for (;;) {
                const { value, done } = await reader.read();
                if (done) break;
                const n = new Uint8Array(buf.length + value.length);
                n.set(buf);
                n.set(value, buf.length);
                buf = n;
                while (buf.length >= 5) {
                    const len = new DataView(buf.buffer, buf.byteOffset).getUint32(1);
                    if (buf.length < 5 + len) break;
                    const type = buf[0];
                    const payload = buf.slice(5, 5 + len);
                    buf = buf.slice(5 + len);
                    if (type === 0) cb.onData(payload);
                    else if (type === 2) {
                        const ex = JSON.parse(dec.decode(payload)) as { code: number; error?: string };
                        closed = true;
                        cb.onExit(ex.code, ex.error ?? "");
                        return;
                    }
                }
            }
            if (!closed) {
                closed = true;
                cb.onExit(-1, "connection lost");
            }
        })().catch((e: Error) => {
            if (!closed) {
                closed = true;
                cb.onExit(-1, e.message);
            }
        });

        return {
            write: (data) => frame(0, enc.encode(data)),
            resize: (c, r) => frame(1, enc.encode(JSON.stringify({ cols: c, rows: r }))),
            close: () => {
                closed = true;
                writer.close().catch(() => {});
            },
        };
    }
}

export const client = new PersonaClient();
