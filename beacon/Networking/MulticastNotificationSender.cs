using System.Text.Json.Nodes;
using System.Threading.Channels;
using Beacon.Shapes;
using Google.Protobuf;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using ModelContextProtocol.Server;

namespace Beacon.Networking;

/// <summary>
/// Subscribes to <see cref="MulticastListener.RequestReceived"/> and forwards each request
/// to the MCP client: every request as a <c>notifications/beacon/multicast</c> notification,
/// and peer commands also as a Claude Code channel event (<c>notifications/claude/channel</c>),
/// which Claude Code puts in front of the model.
/// </summary>
/// <remarks>
/// The event handler only enqueues; sending happens on this service's own loop so the
/// listener's receive loop never waits on the MCP transport.
/// </remarks>
public sealed class MulticastNotificationSender(
    McpServer server,
    MulticastListener listener,
    ILogger<MulticastNotificationSender> logger) : BackgroundService
{
    public const string NotificationMethod = "notifications/beacon/multicast";

    private readonly Channel<MulticastRequestReceivedEventArgs> _pending =
        Channel.CreateBounded<MulticastRequestReceivedEventArgs>(new BoundedChannelOptions(256)
        {
            FullMode = BoundedChannelFullMode.DropOldest,
            SingleReader = true,
        });

    public override Task StartAsync(CancellationToken cancellationToken)
    {
        listener.RequestReceived += OnRequestReceived;
        return base.StartAsync(cancellationToken);
    }

    public override async Task StopAsync(CancellationToken cancellationToken)
    {
        listener.RequestReceived -= OnRequestReceived;
        _pending.Writer.TryComplete();
        await base.StopAsync(cancellationToken);
    }

    private void OnRequestReceived(object? sender, MulticastRequestReceivedEventArgs e) =>
        _pending.Writer.TryWrite(e);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        try
        {
            await foreach (var received in _pending.Reader.ReadAllAsync(stoppingToken))
            {
                try
                {
                    await server.SendNotificationAsync(NotificationMethod, ToParams(received),
                        cancellationToken: stoppingToken);
                    if (ToChannelParams(received) is { } channel)
                    {
                        await server.SendNotificationAsync(ClaudeChannel.Method, channel, cancellationToken: stoppingToken);
                    }
                }
                catch (Exception ex) when (ex is not OperationCanceledException)
                {
                    logger.LogWarning(ex, "Failed to forward multicast request from {Remote}",
                        received.RemoteEndPoint);
                }
            }
        }
        catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
        {
        }
    }

    /// <summary>
    /// Channel event for requests the model should see. Discovery requests are handled by
    /// <see cref="DiscoveryResponder"/> and would only be noise.
    /// </summary>
    private static JsonObject? ToChannelParams(MulticastRequestReceivedEventArgs received) =>
        received.Request.RequestTypeCase switch
        {
            MulticastRequest.RequestTypeOneofCase.Command => new JsonObject
            {
                ["content"] = received.Request.Command.Command,
                ["meta"] = new JsonObject
                {
                    ["kind"] = "command",
                    ["from"] = received.RemoteEndPoint.Address.ToString(),
                },
            },
            _ => null,
        };

    private static JsonObject ToParams(MulticastRequestReceivedEventArgs received) => new()
    {
        ["source"] = received.RemoteEndPoint.ToString(),
        // Protobuf's canonical JSON mapping (camelCase fields, oneof as its set member).
        ["request"] = JsonNode.Parse(JsonFormatter.Default.Format(received.Request)),
    };
}
