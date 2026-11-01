// Browser notifications for what agents report to the host through their
// beacon: report_work_finished and request_input. Every report also shows as
// a toast; with permission it becomes a system notification, which opens the
// spawn's terminal when clicked.

import { useEffect, useState } from "react";
import { Bell, BellOff, BellRing } from "lucide-react";
import { client } from "@/lib/client";
import type { Notice } from "@/lib/types";
import { useToast } from "./Toasts";
import { useWorkbench } from "./Workbench";

const supported = typeof window !== "undefined" && "Notification" in window;

function permission(): NotificationPermission | "unsupported" {
    return supported ? Notification.permission : "unsupported";
}

/** Asks for notification permission (browsers allow it only from a user gesture). */
async function ask(): Promise<NotificationPermission | "unsupported"> {
    if (!supported) return "unsupported";
    if (Notification.permission !== "default") return Notification.permission;
    try {
        return await Notification.requestPermission();
    } catch {
        return Notification.permission;
    }
}

function describe(n: Notice): { title: string; body: string } {
    const where = `${n.persona} · ${n.namespace}`;
    if (n.kind === "finished") return { title: `${n.spawn} finished its work`, body: where };
    let body = n.question ?? "It is waiting for your answer.";
    if (n.options?.length) body += `\n${n.options.join(" / ")}`;
    return { title: `${n.spawn} needs input`, body: `${body}\n${where}` };
}

/** Listens for notices and shows them. Rendered once, inside the workbench. */
export function Notifier() {
    const workbench = useWorkbench();
    const notify = useToast();

    // The first click anywhere asks for permission, if it hasn't been asked.
    useEffect(() => {
        if (permission() !== "default") return;
        const onGesture = () => void ask();
        window.addEventListener("pointerdown", onGesture, { once: true });
        return () => window.removeEventListener("pointerdown", onGesture);
    }, []);

    useEffect(
        () =>
            client.onNotice((n) => {
                const { title, body } = describe(n);
                notify(`${title}${n.kind === "input" && n.question ? `: ${n.question}` : ""}`);
                if (permission() !== "granted") return;
                const sys = new Notification(title, {
                    body,
                    tag: `persona-${n.spawn}-${n.kind}`, // a newer report replaces the older one
                    requireInteraction: n.kind === "input",
                });
                sys.onclick = () => {
                    window.focus();
                    workbench.openTerminal(n.spawn);
                    sys.close();
                };
            }),
        [notify, workbench],
    );
    return null;
}

/** A bell showing the permission; clicking asks for it. */
export function NotificationToggle() {
    const [perm, setPerm] = useState(permission);
    const notify = useToast();
    useEffect(() => {
        // Permission can change in the browser's site settings.
        const t = setInterval(() => setPerm(permission()), 2000);
        return () => clearInterval(t);
    }, []);
    if (perm === "unsupported") return null;
    const Icon = perm === "granted" ? BellRing : perm === "denied" ? BellOff : Bell;
    const label =
        perm === "granted"
            ? "Notifications on: you'll hear when an agent finishes or needs input"
            : perm === "denied"
              ? "Notifications blocked: allow them in the browser's site settings"
              : "Turn on notifications for when an agent finishes or needs input";
    return (
        <button
            title={label}
            onClick={async () => {
                const p = await ask();
                setPerm(p);
                if (p === "denied") notify("Notifications are blocked for this site: allow them in the browser's site settings.", true);
                else if (p === "granted") notify("Notifications on");
            }}
            className={
                "pointer-events-auto rounded-md border px-1.5 py-1 " +
                (perm === "granted" ? "border-emerald-800 text-emerald-300" : perm === "denied" ? "border-red-900 text-red-400" : "border-neutral-700 text-neutral-300 hover:border-violet-500")
            }
        >
            <Icon size={14} />
        </button>
    );
}
