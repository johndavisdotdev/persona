// A small form dialog (Radix Themes): fields in, values out.

import { useEffect, useState } from "react";
import { Button, Dialog, Flex, Select, Text, TextArea, TextField } from "@radix-ui/themes";

export interface Field {
    name: string;
    label: string;
    type?: "text" | "textarea" | "number" | "select";
    placeholder?: string;
    initial?: string;
    required?: boolean;
    /** Must match (the whole value). */
    pattern?: RegExp;
    patternHint?: string;
    options?: { value: string; label: string }[];
    hint?: string;
}

export interface FormDialogSpec {
    title: string;
    description?: string;
    fields: Field[];
    submit: string;
    onSubmit(values: Record<string, string>): void | Promise<void>;
}

export function FormDialog({ spec, onClose }: { spec: FormDialogSpec | null; onClose(): void }) {
    const [values, setValues] = useState<Record<string, string>>({});
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");

    useEffect(() => {
        if (!spec) return;
        const v: Record<string, string> = {};
        for (const f of spec.fields) v[f.name] = f.initial ?? (f.type === "select" ? (f.options?.[0]?.value ?? "") : "");
        setValues(v);
        setError("");
        setBusy(false);
    }, [spec]);

    if (!spec) return null;

    const invalid = spec.fields.find((f) => {
        const v = (values[f.name] ?? "").trim();
        if (f.required && !v) return true;
        return !!v && !!f.pattern && !f.pattern.test(v);
    });

    const submit = async () => {
        if (invalid) return;
        setBusy(true);
        setError("");
        try {
            const out: Record<string, string> = {};
            for (const f of spec.fields) out[f.name] = (values[f.name] ?? "").trim();
            await spec.onSubmit(out);
            onClose();
        } catch (e) {
            setError((e as Error).message);
            setBusy(false);
        }
    };

    return (
        <Dialog.Root open onOpenChange={(open) => !open && onClose()}>
            <Dialog.Content maxWidth="480px">
                <Dialog.Title>{spec.title}</Dialog.Title>
                {spec.description && (
                    <Dialog.Description size="2" mb="3" color="gray">
                        {spec.description}
                    </Dialog.Description>
                )}
                <form
                    onSubmit={(e) => {
                        e.preventDefault();
                        void submit();
                    }}
                >
                    <Flex direction="column" gap="3">
                        {spec.fields.map((f, i) => {
                            const v = values[f.name] ?? "";
                            const bad = !!v && !!f.pattern && !f.pattern.test(v.trim());
                            const set = (x: string) => setValues((s) => ({ ...s, [f.name]: x }));
                            return (
                                <label key={f.name} className="flex flex-col gap-1">
                                    <Text size="2" weight="medium">
                                        {f.label}
                                    </Text>
                                    {f.type === "textarea" ? (
                                        <TextArea value={v} placeholder={f.placeholder} rows={4} onChange={(e) => set(e.target.value)} autoFocus={i === 0} />
                                    ) : f.type === "select" ? (
                                        <Select.Root value={v} onValueChange={set}>
                                            <Select.Trigger />
                                            <Select.Content>
                                                {f.options?.map((o) => (
                                                    <Select.Item key={o.value} value={o.value}>
                                                        {o.label}
                                                    </Select.Item>
                                                ))}
                                            </Select.Content>
                                        </Select.Root>
                                    ) : (
                                        <TextField.Root
                                            value={v}
                                            type={f.type === "number" ? "number" : "text"}
                                            placeholder={f.placeholder}
                                            onChange={(e) => set(e.target.value)}
                                            autoFocus={i === 0}
                                            onFocus={(e) => e.currentTarget.select()}
                                            color={bad ? "red" : undefined}
                                        />
                                    )}
                                    {bad && f.patternHint ? (
                                        <Text size="1" color="red">
                                            {f.patternHint}
                                        </Text>
                                    ) : f.hint ? (
                                        <Text size="1" color="gray">
                                            {f.hint}
                                        </Text>
                                    ) : null}
                                </label>
                            );
                        })}
                        {error && (
                            <Text size="2" color="red">
                                {error}
                            </Text>
                        )}
                        <Flex gap="2" justify="end" mt="2">
                            <Dialog.Close>
                                <Button type="button" variant="soft" color="gray">
                                    Cancel
                                </Button>
                            </Dialog.Close>
                            <Button type="submit" disabled={!!invalid || busy} loading={busy}>
                                {spec.submit}
                            </Button>
                        </Flex>
                    </Flex>
                </form>
            </Dialog.Content>
        </Dialog.Root>
    );
}
