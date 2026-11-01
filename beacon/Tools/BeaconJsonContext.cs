using System.Text.Json;
using System.Text.Json.Serialization;
using ModelContextProtocol;

namespace Beacon.Tools;

/// <summary>
/// Source-generated JSON metadata for tool parameters and results: the published binary
/// is trimmed, which turns off reflection-based serialization.
/// </summary>
[JsonSerializable(typeof(BeaconTools.AgentInfo))]
[JsonSerializable(typeof(BeaconTools.AgentInfo[]))]
[JsonSerializable(typeof(BeaconTools.Delivery))]
[JsonSerializable(typeof(BeaconTools.Delivery[]))]
[JsonSerializable(typeof(string[]))]
[JsonSourceGenerationOptions(JsonSerializerDefaults.Web)]
internal sealed partial class BeaconJsonContext : JsonSerializerContext
{
    // Lazy: a static initializer here would run before the generated Default exists.
    private static JsonSerializerOptions? s_toolOptions;

    public static JsonSerializerOptions ToolOptions => s_toolOptions ??= CreateToolOptions();

    private static JsonSerializerOptions CreateToolOptions()
    {
        var options = new JsonSerializerOptions(McpJsonUtilities.DefaultOptions);
        options.TypeInfoResolverChain.Insert(0, Default);
        options.MakeReadOnly();
        return options;
    }
}
