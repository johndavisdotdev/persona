using System.ComponentModel;
using System.Net;
using System.Net.Sockets;
using Beacon.Networking;
using Beacon.Shapes;
using ModelContextProtocol;
using ModelContextProtocol.Server;

namespace Beacon.Tools;

/// <summary>
/// Tools for reaching other persona agents (multicast on the microVM bridge) and the
/// host broker (through persona-agent).
/// </summary>
[McpServerToolType]
public sealed class BeaconTools(
    MulticastSender multicast,
    UnicastSender unicast,
    AgentDirectory directory,
    BrokerClient broker,
    AgentIdentity identity)
{
    private static readonly TimeSpan RediscoveryWindow = TimeSpan.FromMilliseconds(1500);

    /// <param name="Port">TCP port for send_message; 0 when the agent's beacon takes no direct messages.</param>
    public sealed record AgentInfo(string Name, string Persona, string Address, int Port);

    public sealed record Delivery(string Name, string Persona, string Address, bool Delivered, string? Error);

    [McpServerTool(Name = "discover_agents", ReadOnly = true)]
    [Description("Finds the other persona agents running on this machine. Returns each agent's name, persona, IP address, " +
                 "and the port send_message reaches it on.")]
    public async Task<AgentInfo[]> DiscoverAgents(
        [Description("How long to wait for replies, in milliseconds.")] int timeoutMs = 1500,
        CancellationToken cancellationToken = default)
    {
        var window = TimeSpan.FromMilliseconds(Math.Clamp(timeoutMs, 100, 10_000));
        var found = await multicast.DiscoverAsync(window, cancellationToken);
        return
        [
            .. found.Select(a => new AgentInfo(a.Name, a.Persona, IpAddresses.FromProto(a.Address)?.ToString() ?? "", (int)a.Port))
                .OrderBy(a => a.Name, StringComparer.Ordinal),
        ];
    }

    [McpServerTool(Name = "whoami", ReadOnly = true)]
    [Description("Returns this agent's name, persona, IP address, and direct-message port as other agents see them.")]
    public AgentInfo WhoAmI() =>
        new(identity.Name, identity.Persona, identity.Address?.ToString() ?? "", identity.Port);

    [McpServerTool(Name = "send_message")]
    [Description("Sends a message to specific persona agents: the agent with a name, every agent of a persona, " +
                 "or the agent at an address. Give exactly one of name, persona, or address. " +
                 "Recipients get it as a channel event; returns whether each one acknowledged it.")]
    public async Task<Delivery[]> SendMessage(
        [Description("The message text.")] string message,
        [Description("The recipient agent's name, as discover_agents lists it.")] string? name = null,
        [Description("Send to every agent of this persona (except this agent).")] string? persona = null,
        [Description("The recipient's IP:PORT, or just its IP if discover_agents has seen it.")] string? address = null,
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(message))
        {
            throw new McpException("message must not be empty");
        }

        var selectors = new[] { name, persona, address }.Count(s => !string.IsNullOrWhiteSpace(s));
        if (selectors != 1)
        {
            throw new McpException("give exactly one of name, persona, or address");
        }

        IReadOnlyList<KnownAgent> targets;
        string wanted;
        if (!string.IsNullOrWhiteSpace(address))
        {
            if (!IPEndPoint.TryParse(address.Trim(), out var endPoint))
            {
                throw new McpException($"'{address}' is not an IP address or IP:PORT");
            }

            wanted = $"at {address}";
            targets = endPoint.Port != 0
                ? [directory.ByAddress(endPoint.Address).FirstOrDefault(a => a.Port == endPoint.Port)
                   ?? new KnownAgent("", "", endPoint.Address, endPoint.Port, DateTimeOffset.MinValue)]
                : await Resolve(() => directory.ByAddress(endPoint.Address), cancellationToken);
        }
        else if (!string.IsNullOrWhiteSpace(name))
        {
            wanted = $"named '{name}'";
            targets = await Resolve(() => directory.ByName(name.Trim()), cancellationToken);
        }
        else
        {
            wanted = $"with persona '{persona}'";
            targets = await Resolve(() => directory.ByPersona(persona!.Trim()), cancellationToken);
        }

        if (targets.Count == 0)
        {
            var known = directory.All();
            throw new McpException(known.Count == 0
                ? $"no agent {wanted}: discovery found no agents"
                : $"no agent {wanted}; known agents: " +
                  string.Join(", ", known.Select(a => $"{a.Name} ({a.Persona}) at {a.EndPoint}")));
        }

        return await Task.WhenAll(targets.Select(t => Deliver(t, message, cancellationToken)));
    }

    [McpServerTool(Name = "broadcast_command")]
    [Description("Sends a command to every other persona agent on this machine. They receive it as an MCP notification.")]
    public async Task<string> BroadcastCommand(
        [Description("The command text.")] string command,
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(command))
        {
            throw new McpException("command must not be empty");
        }

        await multicast.SendAsync(new MulticastRequest { Command = new CommandFromGod { Command = command } }, cancellationToken);
        return "sent";
    }

    [McpServerTool(Name = "report_work_finished")]
    [Description("Tells the persona host that this agent has finished its work.")]
    public Task<string> ReportWorkFinished(CancellationToken cancellationToken = default) =>
        SendToBroker(new BrokerMessage { Finished = new WorkFinished() }, cancellationToken);

    [McpServerTool(Name = "request_input")]
    [Description("Asks the user, through the persona host, for input this agent needs to continue. " +
                 "kind is 'confirmation' (yes/no), 'closed' (pick one of choices), or 'open' (free text, choices are suggestions).")]
    public Task<string> RequestInput(
        [Description("The question to ask.")] string question,
        [Description("confirmation, closed, or open.")] string kind = "open",
        [Description("Choices for closed or open questions.")] string[]? choices = null,
        [Description("Label for yes, for confirmation questions.")] string yesText = "Yes",
        [Description("Label for no, for confirmation questions.")] string noText = "No",
        CancellationToken cancellationToken = default)
    {
        if (string.IsNullOrWhiteSpace(question))
        {
            throw new McpException("question must not be empty");
        }

        var need = new NeedInput { Question = question };
        switch (kind.Trim().ToLowerInvariant())
        {
            case "confirmation":
                need.Confirmation = new ConfirmationInput { YesText = yesText, NoText = noText };
                break;
            case "closed":
                if (choices is not { Length: > 0 })
                {
                    throw new McpException("a closed question needs choices");
                }

                need.ClosedSelection = new ClosedSelection { Choices = { choices } };
                break;
            case "open":
                need.OpenSelection = new OpenSelection { Choices = { choices ?? [] } };
                break;
            default:
                throw new McpException($"unknown kind '{kind}': use confirmation, closed, or open");
        }

        return SendToBroker(new BrokerMessage { NeedInput = need }, cancellationToken);
    }

    /// <summary>
    /// Looks the recipients up in the directory, running one discovery round first if it has
    /// none (the directory is empty at startup, and agents come and go). Skips this agent.
    /// </summary>
    private async Task<IReadOnlyList<KnownAgent>> Resolve(Func<IReadOnlyList<KnownAgent>> lookup, CancellationToken cancellationToken)
    {
        var found = Others(lookup());
        if (found.Count == 0)
        {
            await multicast.DiscoverAsync(RediscoveryWindow, cancellationToken);
            found = Others(lookup());
        }

        return found;
    }

    private List<KnownAgent> Others(IReadOnlyList<KnownAgent> agents) =>
        [.. agents.Where(a => !(a.Port == identity.Port && a.Address.Equals(identity.Address)))];

    private async Task<Delivery> Deliver(KnownAgent to, string message, CancellationToken cancellationToken)
    {
        var endPoint = to.EndPoint.ToString();
        if (to.Port == 0)
        {
            return new Delivery(to.Name, to.Persona, to.Address.ToString(), false,
                "this agent's beacon announces no direct-message port (it predates send_message)");
        }

        try
        {
            await unicast.SendAsync(to.EndPoint, message, cancellationToken);
            return new Delivery(to.Name, to.Persona, endPoint, true, null);
        }
        catch (Exception ex) when (ex is IOException or SocketException or TimeoutException or InvalidDataException
                                       or Google.Protobuf.InvalidProtocolBufferException)
        {
            if (ex is SocketException { SocketErrorCode: SocketError.ConnectionRefused })
            {
                // Nothing listens there any more: the agent is gone or restarted on another port.
                directory.Remove(to.EndPoint);
            }

            return new Delivery(to.Name, to.Persona, endPoint, false, ex.Message);
        }
    }

    private async Task<string> SendToBroker(BrokerMessage message, CancellationToken cancellationToken)
    {
        try
        {
            await broker.SendAsync(message, cancellationToken);
            return "delivered to the persona host";
        }
        catch (Exception ex) when (ex is BrokerUnavailableException or IOException or System.Net.Sockets.SocketException)
        {
            throw new McpException(ex.Message, ex);
        }
    }
}
