# persona marketplace (template)

`dotnet publish` copies this tree to `$(MarketplaceDir)` (default: `marketplace/` in the
publish directory) and puts the published `persona-beacon` binary in
`plugins/beacon/bin/`. The result is a Claude Code marketplace named `persona` with one
plugin, `beacon`:

    claude plugin marketplace add ./bin/beacon/marketplace
    claude plugin install beacon@persona
    claude --dangerously-load-development-channels plugin:beacon@persona
