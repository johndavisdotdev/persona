using System.Net;
using System.Net.Sockets;
using Beacon.Shapes;
using Google.Protobuf;
using Microsoft.Extensions.Options;

namespace Beacon.Networking;

/// <summary>Sends requests to the multicast group and runs discovery rounds, whose results go to the <see cref="AgentDirectory"/>.</summary>
public sealed class MulticastSender(IOptions<MulticastOptions> options, AgentIdentity identity, AgentDirectory directory)
{
    private readonly MulticastOptions _options = options.Value;

    private IPEndPoint Group => new(IPAddress.Parse(_options.GroupAddress), _options.Port);

    public async Task SendAsync(MulticastRequest request, CancellationToken cancellationToken)
    {
        using var client = CreateClient();
        await client.SendAsync(request.ToByteArray(), Group, cancellationToken);
    }

    /// <summary>
    /// Multicasts a discovery request from a fresh socket and collects the announcements
    /// peers send back to that socket until <paramref name="window"/> elapses.
    /// </summary>
    public async Task<IReadOnlyList<AgentAnnouncement>> DiscoverAsync(TimeSpan window, CancellationToken cancellationToken)
    {
        using var client = CreateClient();
        var request = new MulticastRequest
        {
            Discovery = new DiscoverAgentsRequest
            {
                ResponseTo = identity.Address is null ? null : IpAddresses.ToProto(identity.Address),
            },
        };
        await client.SendAsync(request.ToByteArray(), Group, cancellationToken);

        // Keyed by address and port: beacons sharing a host (Loopback testing) share an address.
        var found = new Dictionary<(IPAddress, uint), AgentAnnouncement>();
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeout.CancelAfter(window);
        try
        {
            while (true)
            {
                var datagram = await client.ReceiveAsync(timeout.Token);
                AgentAnnouncement announcement;
                try
                {
                    announcement = AgentAnnouncement.Parser.ParseFrom(datagram.Buffer);
                }
                catch (InvalidProtocolBufferException)
                {
                    continue;
                }

                // Trust the address the reply came from over the one it claims.
                announcement.Address = IpAddresses.ToProto(datagram.RemoteEndPoint.Address);
                found[(datagram.RemoteEndPoint.Address, announcement.Port)] = announcement;
            }
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            // The window closed.
        }

        directory.ReplaceAll(found.Values);
        return [.. found.Values];
    }

    private UdpClient CreateClient()
    {
        var group = IPAddress.Parse(_options.GroupAddress);
        var client = new UdpClient(group.AddressFamily);
        try
        {
            var any = group.AddressFamily == AddressFamily.InterNetworkV6 ? IPAddress.IPv6Any : IPAddress.Any;
            client.Client.Bind(new IPEndPoint(any, 0));
            // Stay on the local segment.
            client.Ttl = 1;
            client.MulticastLoopback = _options.Loopback;
            if (!string.IsNullOrWhiteSpace(_options.LocalInterface) && group.AddressFamily == AddressFamily.InterNetwork)
            {
                client.Client.SetSocketOption(SocketOptionLevel.IP, SocketOptionName.MulticastInterface,
                    IPAddress.Parse(_options.LocalInterface).GetAddressBytes());
            }

            return client;
        }
        catch
        {
            client.Dispose();
            throw;
        }
    }
}
