package persona

import (
	"encoding/json"
	"strings"
)

// piExtension reports pi's lifecycle to the status stream through
// persona-status, using Claude Code's event names. pi has no notification or
// task-completed events, so those never appear for pi spawns.
const piExtension = `// persona status stream for pi (installed by persona; see persona manage).
import { spawn } from "node:child_process";

const report = (event: string, payload: object = {}) => {
  try {
    const p = spawn("/usr/local/bin/persona-status", [event], { stdio: ["pipe", "ignore", "ignore"], detached: true });
    p.on("error", () => {});
    p.stdin.on("error", () => {});
    p.stdin.end(JSON.stringify(payload));
    p.unref();
  } catch {}
};

const lastText = (messages: any[]): string | undefined => {
  const m = [...messages].reverse().find((m) => m?.role === "assistant");
  return m?.content?.filter((c: any) => c?.type === "text").map((c: any) => c.text).join("");
};

export default function (pi: any) {
  pi.on("session_start", () => report("SessionStart"));
  pi.on("agent_start", () => report("UserPromptSubmit"));
  pi.on("agent_settled", (_e: any, ctx: any) => {
    let last: string | undefined;
    try {
      last = lastText(ctx.sessionManager.getBranch().filter((x: any) => x.type === "message").map((x: any) => x.message));
    } catch {}
    report("Stop", { last_assistant_message: last });
  });
  pi.on("ui_prompt_start", (e: any) => report("Question", { questions: [{ question: e?.title || "pi is waiting for " + (e?.kind || "your") + " input" }] }));
  pi.on("ui_prompt_end", () => report("UserPromptSubmit"));
}
`

// piSettings is ~/.pi/agent/settings.json before packages are installed
// (pi install adds them). Models come from OpenRouter; a persona model of the
// form provider/model or an OpenRouter model id picks the default.
func piSettings(p *Persona) string {
	s := map[string]any{
		"defaultProvider":     "openrouter",
		"defaultProjectTrust": "always",
		"quietStartup":        true,
	}
	if strings.Contains(p.Model, "/") {
		s["defaultModel"] = p.Model
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return string(b)
}
