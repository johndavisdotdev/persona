# beacon

An MCP server that connects a persona agent to the other persona agents running on the
same machine, and to the persona host.

Agents find each other with UDP multicast (group 239.255.77.1, port 47777, TTL 1) on the
network the persona microVMs share. Inside a microVM the server also reaches the host's
persona broker through persona-agent's socket at `/run/persona/agent.sock`.

## Install

From a directory produced by `dotnet publish` (the `marketplace/` folder):

    claude plugin marketplace add ./marketplace
    /plugin install beacon@persona

## Tools

- `discover_agents`: lists the other agents, with name, persona, IP address and message port
- `whoami`: this agent's name, persona, address and message port
- `send_message`: sends a message to one agent (by name, or by IP:PORT) or to every agent
  of a persona, and reports which recipients acknowledged it
- `broadcast_command`: sends a command to every other agent
- `report_work_finished`: tells the persona host this agent is done
- `request_input`: asks the user, through the persona host, for a decision

Each beacon also listens for direct messages on a free TCP port between 30000 and 32000
(`--Unicast:PortMin` / `--Unicast:PortMax`), which it announces in discovery replies.
Agents seen in discovery rounds, or that sent us a message, are cached, so `send_message`
by name or persona only runs a discovery round when the cache has no match.

## Channel

Commands and direct messages from other agents arrive in the session as channel events,
once the channel is enabled for the session:

    claude --dangerously-load-development-channels plugin:beacon@persona

Settings can be overridden with arguments to the server, e.g.
`--Multicast:GroupAddress=239.1.2.3 --Multicast:Port=5000`.
