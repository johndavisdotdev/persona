namespace Beacon.Networking;

/// <summary>
/// Settings for <see cref="MulticastListener"/>, bound from the "Multicast" configuration section.
/// </summary>
public sealed class MulticastOptions
{
    public const string SectionName = "Multicast";

    /// <summary>Multicast group to join. IPv4 (e.g. 239.x.x.x) or IPv6 (ff0x::...).</summary>
    public string GroupAddress { get; set; } = "239.255.77.1";

    /// <summary>UDP port the group's traffic is sent to.</summary>
    public int Port { get; set; } = 47777;

    /// <summary>
    /// Local interface address to join the group on. Leave empty to let the OS pick.
    /// </summary>
    public string? LocalInterface { get; set; }

    /// <summary>
    /// Deliver our own multicast to sockets on this machine. Off in production (a beacon
    /// never needs to hear itself); on lets several beacons on one host talk, for testing.
    /// </summary>
    public bool Loopback { get; set; }
}
