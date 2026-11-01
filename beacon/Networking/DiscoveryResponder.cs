using System.Net;
using System.Net.Sockets;
using Beacon.Shapes;
using Google.Protobuf;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Beacon.Networking;

/// <summary>Answers peers' discovery requests with this agent's <see cref="AgentAnnouncement"/>.</summary>
public sealed class DiscoveryResponder(
    MulticastListener listener,
    AgentIdentity identity,
    ILogger<DiscoveryResponder> logger) : IHostedService, IDisposable
{
    private readonly UdpClient _client = new(AddressFamily.InterNetwork);

    public Task StartAsync(CancellationToken cancellationToken)
    {
        listener.RequestReceived += OnRequestReceived;
        return Task.CompletedTask;
    }

    public Task StopAsync(CancellationToken cancellationToken)
    {
        listener.RequestReceived -= OnRequestReceived;
        return Task.CompletedTask;
    }

    private void OnRequestReceived(object? sender, MulticastRequestReceivedEventArgs e)
    {
        if (e.Request.RequestTypeCase != MulticastRequest.RequestTypeOneofCase.Discovery)
        {
            return;
        }

        var to = IpAddresses.FromProto(e.Request.Discovery.ResponseTo) ?? e.RemoteEndPoint.Address;
        if (to.AddressFamily != AddressFamily.InterNetwork)
        {
            return;
        }

        try
        {
            // UDP sends don't block on the network, so this is fine on the listener's loop.
            _client.Send(identity.ToAnnouncement().ToByteArray(), new IPEndPoint(to, e.RemoteEndPoint.Port));
        }
        catch (SocketException ex)
        {
            logger.LogWarning(ex, "Could not answer discovery from {Remote}", e.RemoteEndPoint);
        }
    }

    public void Dispose() => _client.Dispose();
}
