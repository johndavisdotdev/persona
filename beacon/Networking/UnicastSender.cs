using System.Net;
using System.Net.Sockets;
using Beacon.Shapes;

namespace Beacon.Networking;

/// <summary>Delivers a <see cref="DirectMessage"/> to one peer's <see cref="UnicastListener"/>.</summary>
public sealed class UnicastSender(AgentIdentity identity)
{
    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);

    /// <summary>Returns once the peer acknowledged the message; throws if it didn't.</summary>
    public async Task SendAsync(IPEndPoint to, string message, CancellationToken cancellationToken)
    {
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeout.CancelAfter(Timeout);
        try
        {
            using var client = new TcpClient(to.AddressFamily);
            await client.ConnectAsync(to, timeout.Token);
            var stream = client.GetStream();
            await Framing.WriteAsync(stream, new DirectMessage
            {
                Message = message,
                From = identity.ToAnnouncement(),
            }, timeout.Token);

            var ack = await Framing.ReadAsync(stream, DirectMessageAck.Parser, timeout.Token);
            if (ack.Error.Length > 0)
            {
                throw new IOException($"{to} refused the message: {ack.Error}");
            }
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw new TimeoutException($"{to} did not acknowledge within {Timeout.TotalSeconds:0}s");
        }
    }
}
