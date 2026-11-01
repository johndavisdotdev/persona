package persona

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Harnesses a persona can run in its spawns.
const (
	HarnessPi     = "pi"     // pi coding agent (https://pi.dev), via OpenRouter
	HarnessClaude = "claude" // Claude Code
)

// DefaultHarness is used when neither the persona nor the base picks one.
const DefaultHarness = HarnessClaude

// Harnesses lists the supported harnesses.
var Harnesses = []string{HarnessPi, HarnessClaude}

// PiPackage is the pi coding agent's npm package.
const PiPackage = "@earendil-works/pi-coding-agent"

// DefaultPiPackages are installed with `pi install` in every pi persona,
// unless the base requirements set their own list.
var DefaultPiPackages = []string{
	"npm:pi-markdown-preview",
	"npm:@companion-ai/feynman",
	"npm:@dietrichgebert/ponytail",
	"npm:bigpowers",
	"npm:projectops",
	"npm:@agimon-ai/po-skill",
	"npm:@agimon-ai/po",
	"npm:@agimon-ai/po-domain",
	"npm:pi-code",
	"npm:pi-docparser",
}

var harnessPackageRe = regexp.MustCompile(`^-?(npm|git|https?):[A-Za-z0-9@/._:+~#=-]{1,200}$`)

// ValidateHarness checks a harness name ("" means the default).
func ValidateHarness(h string) error {
	if h != "" && !slices.Contains(Harnesses, h) {
		return fmt.Errorf("unknown harness %q (supported: %s)", h, strings.Join(Harnesses, ", "))
	}
	return nil
}

// ValidateHarnessPackage checks a pi package spec, e.g. "npm:pi-code"; a
// leading "-" removes a package the base would install.
func ValidateHarnessPackage(s string) error {
	if !harnessPackageRe.MatchString(s) {
		return fmt.Errorf("invalid pi package %q (use npm:<name>, git:<repo> or a URL; prefix with - to remove one)", s)
	}
	return nil
}

// harnessPackages applies a persona's additions and "-" removals to the
// base's package list.
func harnessPackages(base, persona []string) []string {
	out := slices.Clone(base)
	for _, p := range persona {
		if rm, ok := strings.CutPrefix(p, "-"); ok {
			out = slices.DeleteFunc(out, func(x string) bool { return x == rm })
		} else if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
