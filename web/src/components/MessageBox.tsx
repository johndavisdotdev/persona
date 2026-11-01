// A message input that broadcasts into running spawns' Claude Code sessions
// (persona message): to everyone, or to one namespace.

import { useState } from "react";
import { Send } from "lucide-react";
import { client } from "@/lib/client";
import { useToast } from "./Toasts";

interface Props {
    /** null: every running spawn, in every namespace. */
    namespace: string | null;
    /** How many agents would get it (for the placeholder and to disable). */
    recipients: number;
    className?: string;
}

export function MessageBox({ namespace, recipients, className = "" }: Props) {
    const [text, setText] = useState("");
    const [busy, setBusy] = useState(false);
    const notify = useToast();
    const who = namespace ? `the agents in ${namespace}` : "all agents";

    const send = async () => {
        const t = text.trim();
        if (!t || busy) return;
        setBusy(true);
        try {
            await client.request("message", namespace ? { text: t, namespace } : { text: t });
            notify(`Sent to ${who} (${recipients})`);
            setText("");
        } catch (e) {
            notify((e as Error).message, true);
        } finally {
            setBusy(false);
        }
    };

    const none = recipients === 0;
    return (
        <form
            className={"flex items-end gap-2 " + className}
            onSubmit={(e) => {
                e.preventDefault();
                void send();
            }}
        >
            <textarea
                value={text}
                rows={1}
                disabled={none}
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => {
                    // Enter sends; Shift+Enter for a new line.
                    if (e.key === "Enter" && !e.shiftKey) {
                        e.preventDefault();
                        void send();
                    }
                }}
                placeholder={none ? `No running agents${namespace ? ` in ${namespace}` : ""}` : `Message ${who}… (${recipients})`}
                className="max-h-32 min-h-9 flex-1 resize-y rounded-md border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-neutral-100 placeholder:text-neutral-500 focus:border-violet-500 focus:outline-none disabled:opacity-50"
            />
            <button
                type="submit"
                disabled={none || busy || !text.trim()}
                className="flex h-9 items-center gap-1.5 rounded-md bg-violet-600 px-3 text-sm text-white hover:bg-violet-500 disabled:opacity-40 disabled:hover:bg-violet-600"
            >
                <Send size={14} /> {namespace ? "Message namespace" : "Message all"}
            </button>
        </form>
    );
}
