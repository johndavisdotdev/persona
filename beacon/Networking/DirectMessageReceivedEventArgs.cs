using System.Net;
using Beacon.Shapes;

namespace Beacon.Networking;

public sealed class DirectMessageReceivedEventArgs(DirectMessage message, IPEndPoint remoteEndPoint) : EventArgs
{
    public DirectMessage Message { get; } = message;

    /// <summary>The sender's end of the connection (its port is ephemeral, not the one it listens on).</summary>
    public IPEndPoint RemoteEndPoint { get; } = remoteEndPoint;
}
