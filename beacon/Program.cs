using System.Text.Json.Nodes;
using Beacon.Networking;
using Beacon.Tools;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using ModelContextProtocol.Protocol;

var builder = Host.CreateApplicationBuilder(args);

// Configure all logs to go to stderr (stdout is used for the MCP protocol messages).
builder.Logging.AddConsole(o => o.LogToStandardErrorThreshold = LogLevel.Trace);

builder.Services
    .AddMcpServer(options =>
    {
        // Declaring claude/channel makes Claude Code inject our notifications/claude/channel
        // events into the session, once it is started with the channel enabled
        // (--channels / --dangerously-load-development-channels).
        options.Capabilities ??= new ServerCapabilities();
        options.Capabilities.Experimental ??= new Dictionary<string, object>();
        options.Capabilities.Experimental[ClaudeChannel.Capability] = new JsonObject();
        options.ServerInstructions = ClaudeChannel.Instructions;
        // Pin the stateful initialize-era protocol. On 2026-07-28 (stateless,
        // per-request metadata) there is no path for unsolicited server
        // notifications, so Claude Code skips channel events entirely.
        options.ProtocolVersion = "2025-11-25";
    })
    .WithStdioServerTransport()
    .WithTools<BeaconTools>(BeaconJsonContext.ToolOptions);

builder.Services.Configure<MulticastOptions>(builder.Configuration.GetSection(MulticastOptions.SectionName));
builder.Services.Configure<UnicastOptions>(builder.Configuration.GetSection(UnicastOptions.SectionName));
builder.Services.AddSingleton(TimeProvider.System);
builder.Services.AddSingleton<AgentDirectory>();
builder.Services.AddSingleton<AgentIdentity>();
builder.Services.AddSingleton<MulticastSender>();
builder.Services.AddSingleton<UnicastSender>();
builder.Services.AddSingleton<BrokerClient>();
builder.Services.AddSingleton<MulticastListener>();
builder.Services.AddSingleton<UnicastListener>();

// Hosted services start in order. Subscribers come before the listeners so they're
// attached to their events before anything is received, and the unicast listener
// binds its port before discovery requests (which announce it) are answered.
builder.Services.AddHostedService<MulticastNotificationSender>();
builder.Services.AddHostedService<DirectMessageNotificationSender>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<UnicastListener>());
builder.Services.AddHostedService<DiscoveryResponder>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<MulticastListener>());

await builder.Build().RunAsync();
