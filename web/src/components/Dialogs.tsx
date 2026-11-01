import { createContext, useContext, useState, type ReactNode } from "react";
import { FormDialog, type FormDialogSpec } from "./FormDialog";

const DialogContext = createContext<(spec: FormDialogSpec) => void>(() => {});

/** Opens a form dialog from anywhere in the app (including Golden Layout panels). */
export function useFormDialog() {
    return useContext(DialogContext);
}

export function DialogProvider({ children }: { children: ReactNode }) {
    const [spec, setSpec] = useState<FormDialogSpec | null>(null);
    return (
        <DialogContext.Provider value={setSpec}>
            {children}
            <FormDialog spec={spec} onClose={() => setSpec(null)} />
        </DialogContext.Provider>
    );
}
