using System.Globalization;
using System.Text.Json.Nodes;
using System.Threading.Channels;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using ModelContextProtocol.Server;

namespace Beacon.Networking;

/// <summary>
/// Forwards each message from <see cref="UnicastListener.MessageReceived"/> to the MCP client
/// as a Claude Code channel event (<see cref="ClaudeChannel.Method"/>).
/// </summary>
/// <remarks>
/// As in <see cref="MulticastNotificationSender"/>, the handler only enqueues and this
/// service's loop does the sending, so a slow transport never holds a peer's connection.
/// </remarks>
public sealed class DirectMessageNotificationSender(
    McpServer server,
    UnicastListener listener,
    ILogger<DirectMessageNotificationSender> logger) : BackgroundService
{
    private readonly Channel<DirectMessageReceivedEventArgs> _pending =
        Channel.CreateBounded<DirectMessageReceivedEventArgs>(new BoundedChannelOptions(256)
        {
            FullMode = BoundedChannelFullMode.DropOldest,
            SingleReader = true,
        });

    public override Task StartAsync(CancellationToken cancellationToken)
    {
        listener.MessageReceived += OnMessageReceived;
        return base.StartAsync(cancellationToken);
    }

    public override async Task StopAsync(CancellationToken cancellationToken)
    {
        listener.MessageReceived -= OnMessageReceived;
        _pending.Writer.TryComplete();
        await base.StopAsync(cancellationToken);
    }

    private void OnMessageReceived(object? sender, DirectMessageReceivedEventArgs e) =>
        _pending.Writer.TryWrite(e);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        try
        {
            await foreach (var received in _pending.Reader.ReadAllAsync(stoppingToken))
            {
                try
                {
                    await server.SendNotificationAsync(ClaudeChannel.Method, ToChannelParams(received),
                        cancellationToken: stoppingToken);
                }
                catch (Exception ex) when (ex is not OperationCanceledException)
                {
                    logger.LogWarning(ex, "Failed to forward direct message from {Remote}", received.RemoteEndPoint);
                }
            }
        }
        catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
        {
        }
    }

    /// <summary>
    /// The address is the connection's, not the one the sender claims; the port is the one
    /// the sender says it listens on, which is what an answer needs.
    /// </summary>
    internal static JsonObject ToChannelParams(DirectMessageReceivedEventArgs received)
    {
        var from = received.Message.From;
        var meta = new JsonObject
        {
            ["kind"] = "message",
            ["from"] = received.RemoteEndPoint.Address.ToString(),
        };
        if (from is not null)
        {
            if (from.Port != 0)
            {
                meta["from_port"] = from.Port.ToString(CultureInfo.InvariantCulture);
            }

            if (from.Name.Length > 0)
            {
                meta["from_name"] = from.Name;
            }

            if (from.Persona.Length > 0)
            {
                meta["from_persona"] = from.Persona;
            }
        }

        return new JsonObject
        {
            ["content"] = received.Message.Message,
            ["meta"] = meta,
        };
    }
}
