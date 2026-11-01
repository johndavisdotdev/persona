using System.Net;
using System.Net.Sockets;
using Beacon.Shapes;
using Google.Protobuf;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace Beacon.Networking;

/// <summary>
/// Joins a multicast group, decodes each datagram as a <see cref="MulticastRequest"/>,
/// and raises <see cref="RequestReceived"/> for every one that decodes cleanly.
/// </summary>
public sealed class MulticastListener(IOptions<MulticastOptions> options, ILogger<MulticastListener> logger)
    : BackgroundService
{
    private readonly MulticastOptions _options = options.Value;

    /// <summary>
    /// Raised on the listener's receive loop; handlers should return quickly.
    /// </summary>
    public event EventHandler<MulticastRequestReceivedEventArgs>? RequestReceived;

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        var group = IPAddress.Parse(_options.GroupAddress);
        if (!IsMulticast(group))
        {
            throw new InvalidOperationException($"{group} is not a multicast address.");
        }

        using var client = CreateClient(group);
        logger.LogInformation("Listening for multicast requests on {Group}:{Port}", group, _options.Port);

        while (!stoppingToken.IsCancellationRequested)
        {
            UdpReceiveResult datagram;
            try
            {
                datagram = await client.ReceiveAsync(stoppingToken);
            }
            catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
            {
                break;
            }
            catch (SocketException ex)
            {
                logger.LogWarning(ex, "Multicast receive failed");
                continue;
            }

            if (TryDecode(datagram.Buffer, out var request))
            {
                OnRequestReceived(new MulticastRequestReceivedEventArgs(request, datagram.RemoteEndPoint));
            }
            else
            {
                logger.LogDebug("Ignoring {Length}-byte datagram from {Remote}: not a MulticastRequest",
                    datagram.Buffer.Length, datagram.RemoteEndPoint);
            }
        }
    }

    /// <summary>
    /// Protobuf will happily "parse" many arbitrary byte strings (as unknown fields or an
    /// empty message), so a request only counts as decoded if it carries a known request type.
    /// </summary>
    internal static bool TryDecode(ReadOnlySpan<byte> payload, out MulticastRequest request)
    {
        request = null!;
        if (payload.IsEmpty)
        {
            return false;
        }

        try
        {
            var parsed = MulticastRequest.Parser.ParseFrom(payload);
            if (parsed.RequestTypeCase == MulticastRequest.RequestTypeOneofCase.None)
            {
                return false;
            }

            request = parsed;
            return true;
        }
        catch (InvalidProtocolBufferException)
        {
            return false;
        }
    }

    private void OnRequestReceived(MulticastRequestReceivedEventArgs args)
    {
        var handlers = RequestReceived;
        if (handlers is null)
        {
            return;
        }

        // Invoke each handler separately so one faulty subscriber can't starve the rest
        // or kill the receive loop.
        foreach (EventHandler<MulticastRequestReceivedEventArgs> handler in handlers.GetInvocationList())
        {
            try
            {
                handler(this, args);
            }
            catch (Exception ex)
            {
                logger.LogError(ex, "Multicast RequestReceived handler threw");
            }
        }
    }

    private UdpClient CreateClient(IPAddress group)
    {
        var family = group.AddressFamily;
        var client = new UdpClient(family);
        try
        {
            client.Client.SetSocketOption(SocketOptionLevel.Socket, SocketOptionName.ReuseAddress, true);
            var any = family == AddressFamily.InterNetworkV6 ? IPAddress.IPv6Any : IPAddress.Any;
            client.Client.Bind(new IPEndPoint(any, _options.Port));

            if (string.IsNullOrWhiteSpace(_options.LocalInterface))
            {
                client.JoinMulticastGroup(group);
            }
            else
            {
                client.JoinMulticastGroup(group, IPAddress.Parse(_options.LocalInterface));
            }

            return client;
        }
        catch
        {
            client.Dispose();
            throw;
        }
    }

    private static bool IsMulticast(IPAddress address) => address.AddressFamily switch
    {
        AddressFamily.InterNetwork => (address.GetAddressBytes()[0] & 0xF0) == 0xE0,
        AddressFamily.InterNetworkV6 => address.IsIPv6Multicast,
        _ => false,
    };
}
