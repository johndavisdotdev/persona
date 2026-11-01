using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Beacon.Shapes;
using Google.Protobuf;

namespace Beacon.Networking;

/// <summary>
/// Sends <see cref="BrokerMessage"/>s to the host's persona broker.
/// </summary>
/// <remarks>
/// Inside a persona microVM, persona-agent (PID 1) listens on a unix socket and relays
/// guest requests over vsock to the broker on the host. A request is one JSON object
/// <c>{"kind": ..., "body": ...}</c>; the answer is one JSON object
/// <c>{"id": ..., "ok": ..., "error": ..., "body": ...}</c>. Beacon uses kind
/// <c>beacon</c> with the message in protobuf's JSON mapping as the body.
/// </remarks>
public sealed class BrokerClient
{
    public const string Kind = "beacon";
    public const string DefaultSocketPath = "/run/persona/agent.sock";

    private readonly string _socketPath;

    public BrokerClient(Microsoft.Extensions.Configuration.IConfiguration configuration)
        : this(configuration["Broker:SocketPath"] ?? DefaultSocketPath) { }

    public BrokerClient(string socketPath) => _socketPath = socketPath;

    public bool Available => File.Exists(_socketPath);

    public async Task SendAsync(BrokerMessage message, CancellationToken cancellationToken)
    {
        if (!Available)
        {
            throw new BrokerUnavailableException(
                $"No persona agent socket at {_socketPath}: the broker channel only exists inside a persona microVM.");
        }

        var request = new JsonObject
        {
            ["kind"] = Kind,
            ["body"] = JsonNode.Parse(JsonFormatter.Default.Format(message)),
        };

        using var socket = new Socket(AddressFamily.Unix, SocketType.Stream, ProtocolType.Unspecified);
        await socket.ConnectAsync(new UnixDomainSocketEndPoint(_socketPath), cancellationToken);
        await using var stream = new NetworkStream(socket, ownsSocket: false);

        await stream.WriteAsync(Encoding.UTF8.GetBytes(request.ToJsonString() + "\n"), cancellationToken);
        await stream.FlushAsync(cancellationToken);

        using var reader = new StreamReader(stream, Encoding.UTF8);
        var line = await reader.ReadLineAsync(cancellationToken)
                   ?? throw new IOException("persona agent closed the connection without a reply");

        using var reply = JsonDocument.Parse(line);
        var root = reply.RootElement;
        if (!root.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
        {
            var error = root.TryGetProperty("error", out var e) ? e.GetString() : null;
            throw new IOException($"broker refused the message: {error ?? "no reason given"}");
        }
    }
}

public sealed class BrokerUnavailableException(string message) : Exception(message);
