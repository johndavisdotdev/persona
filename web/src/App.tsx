import { useEffect, useState } from "react";
import { Button, Dialog, Flex, Text, TextField, Theme } from "@radix-ui/themes";
import { client } from "@/lib/client";
import { useConnStatus } from "@/lib/hooks";
import { DialogProvider } from "./components/Dialogs";
import { ToastProvider } from "./components/Toasts";
import { Workbench } from "./components/Workbench";

export default function App() {
    useEffect(() => client.start(), []);
    return (
        <Theme appearance="dark" accentColor="violet" grayColor="slate" radius="medium" scaling="95%" className="h-full">
            <ToastProvider>
                <DialogProvider>
                    <div className="flex h-screen flex-col bg-[#0b0d10] text-neutral-200">
                        <Unsupported />
                        <div className="min-h-0 flex-1">
                            <Workbench />
                        </div>
                    </div>
                    <TokenDialog />
                </DialogProvider>
            </ToastProvider>
        </Theme>
    );
}

function Unsupported() {
    const status = useConnStatus();
    if (status.kind !== "unsupported") return null;
    return <div className="bg-red-950 px-4 py-2 text-sm text-red-200">This browser has no WebTransport. Use a recent Chrome, Edge or Firefox.</div>;
}

/** Asks for the access token when the URL didn't carry one (or it was refused). */
function TokenDialog() {
    const status = useConnStatus();
    const [token, setToken] = useState("");
    return (
        <Dialog.Root open={status.kind === "needs-token"}>
            <Dialog.Content maxWidth="440px">
                <Dialog.Title>Access token</Dialog.Title>
                <Dialog.Description size="2" color="gray" mb="3">
                    Open the URL <code>persona server</code> printed, or paste the token from <code>$PERSONA_HOME/server/token</code>.
                </Dialog.Description>
                <form
                    onSubmit={(e) => {
                        e.preventDefault();
                        if (token.trim()) client.provideToken(token.trim());
                    }}
                >
                    <Flex direction="column" gap="3">
                        <TextField.Root autoFocus value={token} onChange={(e) => setToken(e.target.value)} placeholder="token" />
                        <Flex justify="end">
                            <Button type="submit" disabled={!token.trim()}>
                                <Text>Connect</Text>
                            </Button>
                        </Flex>
                    </Flex>
                </form>
            </Dialog.Content>
        </Dialog.Root>
    );
}
