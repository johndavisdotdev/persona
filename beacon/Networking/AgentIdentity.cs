using System.Buffers.Binary;
using System.Net;
using System.Net.NetworkInformation;
using System.Net.Sockets;
using Beacon.Shapes;
using Google.Protobuf;
using Microsoft.Extensions.Options;

namespace Beacon.Networking;

/// <summary>Who this beacon speaks for: the instance's name, persona, address, and direct-message port.</summary>
public sealed class AgentIdentity
{
    private readonly UnicastListener _unicast;

    public AgentIdentity(IOptions<MulticastOptions> options, UnicastListener unicast)
    {
        _unicast = unicast;
        Name = Environment.MachineName;
        Persona = Environment.GetEnvironmentVariable("PERSONA_NAME") ?? "";
        var configured = options.Value.LocalInterface;
        Address = string.IsNullOrWhiteSpace(configured) ? FindLocalAddress() : IPAddress.Parse(configured);
    }

    public string Name { get; }
    public string Persona { get; }

    /// <summary>The address peers can reach us at; null when the host has no usable interface.</summary>
    public IPAddress? Address { get; }

    /// <summary>The TCP port we take direct messages on; 0 until <see cref="UnicastListener"/> has started.</summary>
    public int Port => _unicast.Port;

    public AgentAnnouncement ToAnnouncement() => new()
    {
        Name = Name,
        Persona = Persona,
        Address = Address is null ? null : IpAddresses.ToProto(Address),
        Port = (uint)Port,
    };

    /// <summary>The first IPv4 address on an interface that is up and not loopback (eth0 in a microVM).</summary>
    private static IPAddress? FindLocalAddress() =>
        NetworkInterface.GetAllNetworkInterfaces()
            .Where(n => n.OperationalStatus == OperationalStatus.Up &&
                        n.NetworkInterfaceType != NetworkInterfaceType.Loopback)
            .SelectMany(n => n.GetIPProperties().UnicastAddresses)
            .Select(a => a.Address)
            .FirstOrDefault(a => a.AddressFamily == AddressFamily.InterNetwork);
}

/// <summary>
/// Conversions for the <see cref="IpAddress"/> message. ipv4 holds the address in network
/// byte order read as a number, so 10.200.1.2 is 0x0AC80102.
/// </summary>
public static class IpAddresses
{
    public static IpAddress ToProto(IPAddress address)
    {
        var bytes = address.GetAddressBytes();
        return address.AddressFamily == AddressFamily.InterNetwork
            ? new IpAddress { Ipv4 = BinaryPrimitives.ReadUInt32BigEndian(bytes) }
            : new IpAddress { Ipv6 = ByteString.CopyFrom(bytes) };
    }

    public static IPAddress? FromProto(IpAddress? address)
    {
        switch (address?.AddressCase)
        {
            case IpAddress.AddressOneofCase.Ipv4:
                Span<byte> v4 = stackalloc byte[4];
                BinaryPrimitives.WriteUInt32BigEndian(v4, address.Ipv4);
                return new IPAddress(v4);
            case IpAddress.AddressOneofCase.Ipv6 when address.Ipv6.Length == 16:
                return new IPAddress(address.Ipv6.Span);
            default:
                return null;
        }
    }
}
