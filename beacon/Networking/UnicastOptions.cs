namespace Beacon.Networking;

/// <summary>
/// Settings for <see cref="UnicastListener"/>, bound from the "Unicast" configuration section.
/// </summary>
public sealed class UnicastOptions
{
    public const string SectionName = "Unicast";

    /// <summary>Lowest TCP port the listener may take.</summary>
    public int PortMin { get; set; } = 30000;

    /// <summary>Highest TCP port the listener may take.</summary>
    public int PortMax { get; set; } = 32000;
}
