using System.Net;
using Beacon.Shapes;

namespace Beacon.Networking;

public sealed class MulticastRequestReceivedEventArgs(MulticastRequest request, IPEndPoint remoteEndPoint) : EventArgs
{
    public MulticastRequest Request { get; } = request;

    /// <summary>The sender of the datagram.</summary>
    public IPEndPoint RemoteEndPoint { get; } = remoteEndPoint;
}
