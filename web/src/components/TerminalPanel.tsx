// An xterm.js terminal, either on a spawn's Claude Code session (interactive)
// or showing a persona CLI job's output (read-only).

import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import type { ComponentContainer } from "golden-layout";
import { client, type StreamHandle } from "@/lib/client";
import { useToast } from "./Toasts";
import type { JobSpec } from "./Workbench";

interface Props {
    container: ComponentContainer;
    spawn?: string;
    job?: JobSpec & { title: string };
}

export function TerminalPanel({ container, spawn, job }: Props) {
    const hostRef = useRef<HTMLDivElement>(null);
    const notify = useToast();

    useEffect(() => {
        const interactive = !!spawn;
        const term = new Terminal({
            cursorBlink: interactive,
            disableStdin: !interactive,
            convertEol: !interactive,
            fontFamily: '"JetBrains Mono", "SF Mono", Menlo, Consolas, monospace',
            fontSize: 13,
            scrollback: 10000,
            theme: { background: "#0b0d10", foreground: "#d7dde6", cursor: "#a78bfa", selectionBackground: "#3b3466" },
        });
        const fit = new FitAddon();
        term.loadAddon(fit);
        term.open(hostRef.current!);
        fit.fit();

        let handle: StreamHandle | null = null;
        let disposed = false;
        const header = spawn ? { type: "attach", spawn } : { type: "job", kind: job!.kind, args: job!.args };
        client
            .openStream(header, term.cols, term.rows, {
                onData: (b) => term.write(b),
                onExit: (code, error) => {
                    if (disposed) return;
                    if (error) term.write(`\r\n\x1b[31m${error}\x1b[0m`);
                    const what = interactive ? "detached" : code === 0 ? "finished" : `failed: exit ${code}`;
                    term.write(`\r\n\x1b[2m[${what}]\x1b[0m\r\n`);
                    container.setTitle((interactive ? spawn! : job!.title) + (interactive || code === 0 ? " ·" : " ✗"));
                    if (job) notify(code === 0 ? `${job.title}: done` : `${job.title}: failed`, code !== 0);
                },
            })
            .then((h) => {
                if (disposed) return h.close();
                handle = h;
                if (interactive) term.onData((d) => h.write(d));
                term.onResize(({ cols, rows }) => h.resize(cols, rows));
                term.focus();
            })
            .catch((e: Error) => term.write(`\x1b[31m${e.message}\x1b[0m\r\n`));

        // Golden Layout resizes the container when it is split, docked or
        // moved; fit the terminal to it.
        const ro = new ResizeObserver(() => {
            if (hostRef.current && hostRef.current.offsetWidth > 0) fit.fit();
        });
        ro.observe(hostRef.current!);
        const onShow = () => requestAnimationFrame(() => fit.fit());
        container.on("show", onShow);

        return () => {
            disposed = true;
            ro.disconnect();
            container.off("show", onShow);
            handle?.close(); // an attach only detaches: the session keeps running
            term.dispose();
        };
    }, [spawn, job, container, notify]);

    return <div ref={hostRef} className="h-full w-full bg-[#0b0d10] p-1.5" />;
}
