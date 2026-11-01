// Port forwards: list, add, remove (persona fw).

import { Button, Table, Badge } from "@radix-ui/themes";
import { Plus, Trash2 } from "lucide-react";
import { client } from "@/lib/client";
import { useServerState } from "@/lib/hooks";
import { forwardFrom, isCGNAT } from "@/lib/types";
import { useFormDialog } from "./Dialogs";
import { useToast } from "./Toasts";

export function FirewallPanel() {
    const state = useServerState();
    const openForm = useFormDialog();
    const notify = useToast();

    if (!state.features.firewall) {
        return <p className="p-4 text-sm text-neutral-400">Port forwards need the firecracker runtime.</p>;
    }
    const running = state.spawns.filter((s) => s.state === "running");

    const add = () =>
        openForm({
            title: "Add a port forward",
            submit: "Add rule",
            fields: [
                {
                    name: "host",
                    label: "Expose on",
                    type: "select",
                    options: [
                        { value: "localhost", label: "localhost (this machine)" },
                        { value: "cgnat", label: "other spawns (100.64.x.x, every namespace)" },
                        { value: "ip", label: "a host IP address (below)" },
                    ],
                },
                { name: "ip", label: "Host IP (for “a host IP address”)", placeholder: "172.16.1.100", pattern: /^\d{1,3}(\.\d{1,3}){3}$/, patternHint: "an IPv4 address" },
                { name: "hostPort", label: "Port", type: "number", required: true, pattern: /^\d{1,5}$/ },
                {
                    name: "proto",
                    label: "Protocol",
                    type: "select",
                    options: [
                        { value: "tcp", label: "TCP" },
                        { value: "udp", label: "UDP" },
                    ],
                },
                { name: "spawn", label: "To spawn", type: "select", options: running.map((s) => ({ value: s.name, label: `${s.name} (${s.namespace})` })) },
                { name: "port", label: "Spawn port", type: "number", required: true, pattern: /^\d{1,5}$/ },
            ],
            onSubmit: async ({ host, ip, hostPort, proto, spawn, port }) => {
                const h = host === "ip" ? ip : host;
                if (!h) throw new Error("enter the host IP address");
                if (!spawn) throw new Error("no running spawn to forward to");
                const r = await client.request<{ id: number }>("fw.add", { from: `${h}:${hostPort}:${proto}`, spawn, port: +port });
                notify(`Rule ${r.id} added`);
            },
        });

    return (
        <div className="flex h-full flex-col gap-3 overflow-auto p-3">
            <div className="flex items-center gap-2">
                <span className="text-xs tracking-wider text-neutral-400 uppercase">Port forwards</span>
                <span className="flex-1" />
                <Button size="1" onClick={add} disabled={running.length === 0}>
                    <Plus size={13} /> Add rule
                </Button>
            </div>
            {state.forwards.length === 0 ? (
                <p className="text-sm text-neutral-500">No port forwards. Right-click a spawn in the diagram, or Add rule.</p>
            ) : (
                <Table.Root size="1" variant="surface">
                    <Table.Header>
                        <Table.Row>
                            <Table.ColumnHeaderCell>ID</Table.ColumnHeaderCell>
                            <Table.ColumnHeaderCell>From</Table.ColumnHeaderCell>
                            <Table.ColumnHeaderCell>To</Table.ColumnHeaderCell>
                            <Table.ColumnHeaderCell>Namespace</Table.ColumnHeaderCell>
                            <Table.ColumnHeaderCell />
                        </Table.Row>
                    </Table.Header>
                    <Table.Body>
                        {state.forwards.map((f) => (
                            <Table.Row key={f.id}>
                                <Table.Cell>{f.id}</Table.Cell>
                                <Table.Cell className="font-mono">
                                    {forwardFrom(f)}{" "}
                                    {isCGNAT(f.host_ip) && (
                                        <Badge color="violet" size="1">
                                            spawns
                                        </Badge>
                                    )}
                                </Table.Cell>
                                <Table.Cell className="font-mono">
                                    {f.spawn}:{f.port}
                                </Table.Cell>
                                <Table.Cell>{f.namespace}</Table.Cell>
                                <Table.Cell>
                                    <Button
                                        size="1"
                                        variant="ghost"
                                        color="red"
                                        onClick={() =>
                                            client.request("fw.remove", { id: f.id }).then(
                                                () => notify(`Removed rule ${f.id}`),
                                                (e: Error) => notify(e.message, true),
                                            )
                                        }
                                    >
                                        <Trash2 size={13} />
                                    </Button>
                                </Table.Cell>
                            </Table.Row>
                        ))}
                    </Table.Body>
                </Table.Root>
            )}
        </div>
    );
}
