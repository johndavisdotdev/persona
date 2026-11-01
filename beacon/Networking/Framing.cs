using Google.Protobuf;

namespace Beacon.Networking;

/// <summary>
/// One protobuf message per frame, prefixed by its length as a varint (the format of
/// <see cref="MessageExtensions.WriteDelimitedTo"/>), read with a size cap so a peer
/// can't make us allocate whatever length it claims.
/// </summary>
internal static class Framing
{
    public const int MaxMessageBytes = 1 << 20;

    public static async Task WriteAsync(Stream stream, IMessage message, CancellationToken cancellationToken)
    {
        using var buffer = new MemoryStream();
        message.WriteDelimitedTo(buffer);
        await stream.WriteAsync(buffer.GetBuffer().AsMemory(0, (int)buffer.Length), cancellationToken);
        await stream.FlushAsync(cancellationToken);
    }

    public static async Task<T> ReadAsync<T>(Stream stream, MessageParser<T> parser, CancellationToken cancellationToken)
        where T : IMessage<T>
    {
        var length = 0;
        var one = new byte[1];
        for (var shift = 0; ; shift += 7)
        {
            if (shift > 28)
            {
                throw new InvalidDataException("frame length varint is too long");
            }

            await stream.ReadExactlyAsync(one, cancellationToken);
            length |= (one[0] & 0x7F) << shift;
            if ((one[0] & 0x80) == 0)
            {
                break;
            }
        }

        if (length is < 0 or > MaxMessageBytes)
        {
            throw new InvalidDataException($"frame of {length} bytes exceeds the {MaxMessageBytes}-byte limit");
        }

        var payload = new byte[length];
        await stream.ReadExactlyAsync(payload, cancellationToken);
        return parser.ParseFrom(payload);
    }
}
