import { createContext, useCallback, useContext, useState, type ReactNode } from "react";

interface Toast {
    id: number;
    text: string;
    error: boolean;
}

type Notify = (text: string, error?: boolean) => void;

const ToastContext = createContext<Notify>(() => {});

/** Shows short notices in the corner; errors stay longer. */
export function useToast() {
    return useContext(ToastContext);
}

let seq = 0;

export function ToastProvider({ children }: { children: ReactNode }) {
    const [toasts, setToasts] = useState<Toast[]>([]);
    const notify = useCallback<Notify>((text, error = false) => {
        const id = ++seq;
        setToasts((t) => [...t, { id, text, error }]);
        setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), error ? 9000 : 4000);
    }, []);
    return (
        <ToastContext.Provider value={notify}>
            {children}
            <div className="pointer-events-none fixed top-12 right-4 z-50 flex flex-col gap-2">
                {toasts.map((t) => (
                    <div
                        key={t.id}
                        className={
                            "pointer-events-auto max-w-md rounded-lg border border-neutral-700 bg-neutral-900 px-3 py-2 text-sm whitespace-pre-wrap text-neutral-100 shadow-xl " +
                            (t.error ? "border-l-4 border-l-red-500" : "border-l-4 border-l-emerald-500")
                        }
                    >
                        {t.text}
                    </div>
                ))}
            </div>
        </ToastContext.Provider>
    );
}
