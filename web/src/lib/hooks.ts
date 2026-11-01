import { useSyncExternalStore } from "react";
import { client } from "./client";

/** The latest state the server pushed; re-renders on every change. */
export function useServerState() {
    return useSyncExternalStore(client.subscribe, client.getState);
}

export function useConnStatus() {
    return useSyncExternalStore(client.subscribe, client.getStatus);
}
