package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxFlakeRebuilds caps how many times a FlakeFixer rebuilds the image.
const MaxFlakeRebuilds = 5

// FlakeFixer is an agent that repairs a persona whose image build failed: it
// reads the build log, edits flake.nix, and rebuilds, in one conversation, so
// each new failure is seen in the light of the earlier attempts. It gives up
// after MaxFlakeRebuilds rebuilds, or when the failure isn't the flake's
// (Docker down, the network, a setup step unrelated to packages).
type FlakeFixer struct {
	Client   *Client
	Progress func(string)
}

// FlakeFixInput is the failed build and what the agent may do about it.
type FlakeFixInput struct {
	Name  string // persona name
	Flake string // the flake.nix that failed
	Log   string // tail of the failed build's output, ending with its error

	// Rebuild writes flake to flake.nix and builds the image, returning the
	// build output's tail and whether it failed.
	Rebuild func(ctx context.Context, flake string) (log string, err error)
	// ReadFile reads one of the persona's files (persona.yaml, the setup
	// scripts); nil disables the tool.
	ReadFile func(name string) (string, error)
	// ReadableFiles are the names ReadFile accepts.
	ReadableFiles []string
	// CheckAttrs reports, for each nixpkgs attribute path, whether it exists
	// in the persona's locked nixpkgs and its version; nil disables the tool.
	CheckAttrs func(ctx context.Context, attrs []string) (string, error)
	// MaxRebuilds overrides MaxFlakeRebuilds when positive.
	MaxRebuilds int
}

// FlakeFixResult says how the repair ended.
type FlakeFixResult struct {
	Fixed    bool
	Rebuilds int
	Summary  string // the agent's explanation of the fix, or why it gave up
}

const flakeFixSystem = `You repair a persona's container image build. A persona is a coding-agent
image: flake.nix builds the base layer with Nix (dockerTools.buildLayeredImage
over nixpkgs), then setup-root.sh and setup-user.sh run in a container made
from it (installing the agent harness, plugins, npm/uv/cargo tools), and the
container is committed.

The build failed. Find the cause in the log and fix it by changing flake.nix,
then call rebuild with the complete new flake.nix. You may rebuild at most
%d times in total; each rebuild returns the new outcome.

Rules:
- Change flake.nix only, and as little as possible: keep its structure and
  helpers (quiet, attr, newest, withPip, libsOf, one, ...), the image config
  and the fakeRootCommands. Never remove the base packages the image relies on
  (bash, coreutils, curl, git, jq, tmux, ...) or the Node and Python runtimes:
  npx and uvx must stay.
- Typical fixes: a renamed or removed nixpkgs attribute (use the new name, or
  an "a|b" alternative where the flake uses attr), a package that is broken,
  insecure or unfree in this nixpkgs (swap it for an equivalent, or drop it if
  nothing needs it), a version attribute that no longer exists (pick an
  existing one), an evaluation error in an expression, or a command the setup
  scripts need that the image lacks (add the package that provides it).
- Use check_attrs to confirm attribute names before rebuilding rather than
  guessing; it evaluates against this persona's locked nixpkgs.
- If the failure can't be fixed in flake.nix (Docker or the network is down, a
  download server failed, a setup step fails for reasons unrelated to the
  image's packages), call give_up and say what the user should do instead.
  Do not rebuild hoping a transient error goes away more than once.`

// Fix runs the repair. It returns an error only when the agent itself fails
// (API errors); a repair that doesn't succeed is a result with Fixed false.
func (f *FlakeFixer) Fix(ctx context.Context, in FlakeFixInput) (FlakeFixResult, error) {
	max := in.MaxRebuilds
	if max <= 0 {
		max = MaxFlakeRebuilds
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string) {}
	}
	tools := []Tool{
		{Name: "rebuild", Description: "Write flake.nix and rebuild the image. Returns whether the build succeeded and, if not, the tail of its log.",
			InputSchema: map[string]any{"type": "object", "required": []string{"flake", "change"}, "properties": map[string]any{
				"flake":  map[string]any{"type": "string", "description": "the complete new flake.nix"},
				"change": map[string]any{"type": "string", "description": "one sentence: what you changed and why"},
			}}},
		{Name: "give_up", Description: "Stop: the failure can't be fixed in flake.nix, or you're out of ideas.",
			InputSchema: map[string]any{"type": "object", "required": []string{"reason"}, "properties": map[string]any{
				"reason": map[string]any{"type": "string", "description": "the cause, and what the user should do"},
			}}},
	}
	if in.CheckAttrs != nil {
		tools = append(tools, Tool{Name: "check_attrs", Description: "Check nixpkgs attribute paths (e.g. \"python313\", \"beamPackages.elixir\", \"libpq.dev\") in this persona's locked nixpkgs: whether each exists, evaluates, and its version.",
			InputSchema: map[string]any{"type": "object", "required": []string{"attrs"}, "properties": map[string]any{
				"attrs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 50},
			}}})
	}
	if in.ReadFile != nil {
		tools = append(tools, Tool{Name: "read_file", Description: "Read one of the persona's other files: " + strings.Join(in.ReadableFiles, ", ") + ".",
			InputSchema: map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{
				"name": map[string]any{"type": "string", "enum": in.ReadableFiles},
			}}})
	}

	var res FlakeFixResult
	handle := func(ctx context.Context, name string, raw json.RawMessage) (string, bool, error) {
		switch name {
		case "rebuild":
			var a struct{ Flake, Change string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", false, err
			}
			if strings.TrimSpace(a.Flake) == "" {
				return "", false, errors.New("flake is empty: pass the complete flake.nix")
			}
			if res.Rebuilds >= max {
				return "", true, nil // out of rebuilds; the loop ends with Fixed false
			}
			res.Rebuilds++
			progress(fmt.Sprintf("Rebuild %d/%d: %s", res.Rebuilds, max, a.Change))
			log, err := in.Rebuild(ctx, a.Flake)
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			if err == nil {
				res.Fixed, res.Summary = true, a.Change
				return "The build succeeded.", true, nil
			}
			res.Summary = a.Change
			if res.Rebuilds >= max {
				return "The build failed again, and that was the last rebuild.", true, nil
			}
			return fmt.Sprintf("The build failed (%d of %d rebuilds used). Log tail:\n%s", res.Rebuilds, max, log), false, nil
		case "give_up":
			var a struct{ Reason string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", false, err
			}
			res.Summary = a.Reason
			return "ok", true, nil
		case "check_attrs":
			var a struct{ Attrs []string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", false, err
			}
			progress("Checking " + strings.Join(a.Attrs, ", "))
			r, err := in.CheckAttrs(ctx, a.Attrs)
			return r, false, err
		case "read_file":
			var a struct{ Name string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", false, err
			}
			r, err := in.ReadFile(a.Name)
			return r, false, err
		}
		return "", false, fmt.Errorf("unknown tool %q", name)
	}

	user := fmt.Sprintf("Persona %q failed to build.\n\n<flake.nix>\n%s\n</flake.nix>\n\n<build-log-tail>\n%s\n</build-log-tail>",
		in.Name, in.Flake, in.Log)
	// Each rebuild takes a turn, plus a few for reading and checking per rebuild.
	if err := f.Client.RunTools(ctx, fmt.Sprintf(flakeFixSystem, max), user, tools, handle, max*6+4); err != nil {
		if res.Rebuilds > 0 && !res.Fixed && strings.Contains(err.Error(), "did not finish") {
			return res, nil // ran out of turns between rebuilds
		}
		return res, err
	}
	return res, nil
}
