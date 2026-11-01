using System.Net;
using Beacon.Shapes;

namespace Beacon.Networking;

/// <summary>An agent we have heard from, and where it takes direct messages.</summary>
public sealed record KnownAgent(string Name, string Persona, IPAddress Address, int Port, DateTimeOffset LastSeen)
{
    public IPEndPoint EndPoint => new(Address, Port);
}

/// <summary>
/// The agents learned from discovery rounds and incoming direct messages, so messages can
/// be addressed by name or persona without a discovery round each time.
/// </summary>
public sealed class AgentDirectory(TimeProvider time)
{
    private readonly Lock _lock = new();
    private readonly Dictionary<IPEndPoint, KnownAgent> _agents = [];

    public AgentDirectory() : this(TimeProvider.System) { }

    /// <summary>A discovery round is a full census: agents that didn't answer are forgotten.</summary>
    public void ReplaceAll(IEnumerable<AgentAnnouncement> announcements)
    {
        var now = time.GetUtcNow();
        lock (_lock)
        {
            _agents.Clear();
            foreach (var announcement in announcements)
            {
                if (IpAddresses.FromProto(announcement.Address) is { } address)
                {
                    Add(announcement, address, now);
                }
            }
        }
    }

    /// <summary>Records one agent, at <paramref name="address"/> rather than the one it claims.</summary>
    public void Upsert(AgentAnnouncement announcement, IPAddress address)
    {
        var now = time.GetUtcNow();
        lock (_lock)
        {
            Add(announcement, address, now);
        }
    }

    public void Remove(IPEndPoint endPoint)
    {
        lock (_lock)
        {
            _agents.Remove(endPoint);
        }
    }

    public IReadOnlyList<KnownAgent> All()
    {
        lock (_lock)
        {
            return [.. _agents.Values];
        }
    }

    public IReadOnlyList<KnownAgent> ByName(string name) =>
        Where(a => string.Equals(a.Name, name, StringComparison.OrdinalIgnoreCase));

    public IReadOnlyList<KnownAgent> ByPersona(string persona) =>
        Where(a => string.Equals(a.Persona, persona, StringComparison.OrdinalIgnoreCase));

    public IReadOnlyList<KnownAgent> ByAddress(IPAddress address) => Where(a => a.Address.Equals(address));

    private IReadOnlyList<KnownAgent> Where(Func<KnownAgent, bool> predicate)
    {
        lock (_lock)
        {
            return [.. _agents.Values.Where(predicate)];
        }
    }

    private void Add(AgentAnnouncement announcement, IPAddress address, DateTimeOffset now)
    {
        var port = announcement.Port <= IPEndPoint.MaxPort ? (int)announcement.Port : 0;
        var agent = new KnownAgent(announcement.Name, announcement.Persona, address, port, now);

        // One agent reached at another of its addresses: keep only the latest, so it isn't messaged twice.
        foreach (var stale in _agents.Values.Where(a => a.Port == port && a.Name == agent.Name &&
                                                        !a.Address.Equals(address)).ToList())
        {
            _agents.Remove(stale.EndPoint);
        }

        _agents[agent.EndPoint] = agent;
    }
}
