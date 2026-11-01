using System.Net;
using System.Net.Sockets;
using Beacon.Shapes;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace Beacon.Networking;

/// <summary>
/// Takes <see cref="DirectMessage"/>s from peers over TCP, on a free port in the configured
/// range, and raises <see cref="MessageReceived"/> for each. The port is announced to peers
/// in <see cref="AgentAnnouncement.Port"/>.
/// </summary>
public sealed class UnicastListener(
    IOptions<UnicastOptions> options,
    AgentDirectory directory,
    ILogger<UnicastListener> logger)
    : BackgroundService
{
    private static readonly TimeSpan ConnectionTimeout = TimeSpan.FromSeconds(10);

    private readonly UnicastOptions _options = options.Value;
    private TcpListener? _listener;

    /// <summary>The port we listen on; 0 until the service has started.</summary>
    public int Port { get; private set; }

    /// <summary>Raised on the connection's task; handlers should return quickly.</summary>
    public event EventHandler<DirectMessageReceivedEventArgs>? MessageReceived;

    /// <summary>Binds before the host starts later services, so the port is known when discovery is answered.</summary>
    public override Task StartAsync(CancellationToken cancellationToken)
    {
        _listener = Bind();
        Port = ((IPEndPoint)_listener.LocalEndpoint).Port;
        logger.LogInformation("Listening for direct messages on TCP port {Port}", Port);
        return base.StartAsync(cancellationToken);
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        var listener = _listener ?? throw new InvalidOperationException("not started");
        try
        {
            while (!stoppingToken.IsCancellationRequested)
            {
                Socket socket;
                try
                {
                    socket = await listener.AcceptSocketAsync(stoppingToken);
                }
                catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
                {
                    break;
                }
                catch (SocketException ex)
                {
                    logger.LogWarning(ex, "Accepting a direct-message connection failed");
                    continue;
                }

                _ = HandleAsync(socket, stoppingToken);
            }
        }
        finally
        {
            listener.Stop();
        }
    }

    private async Task HandleAsync(Socket socket, CancellationToken stoppingToken)
    {
        var remote = (IPEndPoint)socket.RemoteEndPoint!;
        try
        {
            using var _ = socket;
            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(stoppingToken);
            timeout.CancelAfter(ConnectionTimeout);
            await using var stream = new NetworkStream(socket, ownsSocket: false);

            var message = await Framing.ReadAsync(stream, DirectMessage.Parser, timeout.Token);
            var ack = new DirectMessageAck();
            if (string.IsNullOrWhiteSpace(message.Message))
            {
                ack.Error = "empty message";
            }
            else
            {
                // Remember the sender, so an answer can be addressed by its name.
                if (message.From is { Port: not 0 } from)
                {
                    directory.Upsert(from, remote.Address);
                }

                OnMessageReceived(new DirectMessageReceivedEventArgs(message, remote));
            }

            await Framing.WriteAsync(stream, ack, timeout.Token);
            socket.Shutdown(SocketShutdown.Both);
        }
        catch (Exception ex) when (ex is IOException or SocketException or InvalidDataException
                                       or Google.Protobuf.InvalidProtocolBufferException or OperationCanceledException)
        {
            logger.LogDebug(ex, "Dropped direct-message connection from {Remote}", remote);
        }
    }

    private void OnMessageReceived(DirectMessageReceivedEventArgs args)
    {
        var handlers = MessageReceived;
        if (handlers is null)
        {
            return;
        }

        foreach (EventHandler<DirectMessageReceivedEventArgs> handler in handlers.GetInvocationList())
        {
            try
            {
                handler(this, args);
            }
            catch (Exception ex)
            {
                logger.LogError(ex, "Unicast MessageReceived handler threw");
            }
        }
    }

    /// <summary>Tries every port in the range once, starting at a random one so agents spread out.</summary>
    private TcpListener Bind()
    {
        var min = _options.PortMin;
        var max = _options.PortMax;
        if (min is < 1 or > 65535 || max < min || max > 65535)
        {
            throw new InvalidOperationException($"invalid unicast port range {min}-{max}");
        }

        var count = max - min + 1;
        var start = Random.Shared.Next(count);
        for (var i = 0; i < count; i++)
        {
            var listener = new TcpListener(IPAddress.Any, min + (start + i) % count);
            try
            {
                listener.Start();
                return listener;
            }
            catch (SocketException ex) when (ex.SocketErrorCode is SocketError.AddressAlreadyInUse or SocketError.AccessDenied)
            {
                listener.Dispose();
            }
        }

        throw new InvalidOperationException($"no free TCP port in {min}-{max}");
    }

    public override void Dispose()
    {
        _listener?.Dispose();
        base.Dispose();
    }
}
