package persona

// The status stream: every harness reports lifecycle events through
// persona-status (installed in every image), which hands them to
// persona-agent in a microVM or appends them to the mounted status directory
// in a container. See package status for the format and persona manage.

// StatusScript is /usr/local/bin/persona-status. It reads a hook payload
// (Claude Code's JSON, or the same fields from another harness) on stdin and
// reports one event. It never fails or blocks the agent.
const StatusScript = `#!/bin/sh
# persona-status EVENT: report a harness lifecycle event (hook JSON on stdin).
ev="${1:-}"
[ -n "$ev" ] || exit 0
line=$(jq -c --arg e "$ev" --arg h "${PERSONA_HARNESS:-claude}" '
  def clip(n): if type == "string" then .[0:n] else null end;
  {event: $e, harness: $h, ts: (now | todate),
   notification_type, title: (.title | clip(200)), message: (.message | clip(2000)),
   last_message: (.last_assistant_message | clip(4000)),
   questions: (.tool_input.questions // .questions // null),
   task: (.task_subject | clip(500)), prompt: (.prompt | clip(1000)),
   error: (.error | if type == "string" then .[0:1000] elif type == "object" then ((.message // tojson) | .[0:1000]) else null end)}
  | with_entries(select(.value != null))' 2>/dev/null) || exit 0
[ -n "$line" ] || exit 0
if [ -S /run/persona/agent.sock ] && command -v persona-agent >/dev/null 2>&1; then
  printf '%s\n' "$line" | persona-agent status >/dev/null 2>&1
elif [ -d /run/persona-status ] && [ -w /run/persona-status ]; then
  printf '%s\n' "$line" >> /run/persona-status/events.jsonl
fi
exit 0
`

// statusHookCommand reports event via persona-status.
func statusHookCommand(event string) map[string]any {
	return map[string]any{"type": "command", "command": "/usr/local/bin/persona-status " + event, "async": true, "timeout": 10}
}

// claudeStatusHooks are the Claude Code hooks that feed the status stream.
// AskUserQuestion is caught before it runs so the question shows up while
// the agent waits for an answer.
func claudeStatusHooks() map[string][]map[string]any {
	group := func(event string) map[string]any {
		return map[string]any{"hooks": []any{statusHookCommand(event)}}
	}
	return map[string][]map[string]any{
		"SessionStart":     {group("SessionStart")},
		"UserPromptSubmit": {group("UserPromptSubmit")},
		"Stop":             {group("Stop")},
		"StopFailure":      {group("StopFailure")}, // the turn ended in an API error
		"Notification":     {group("Notification")},
		"TaskCompleted":    {group("TaskCompleted")},
		"PreToolUse":       {{"matcher": "AskUserQuestion", "hooks": []any{statusHookCommand("Question")}}},
	}
}

// addStatusHooks appends the status hooks to settings' hooks, keeping any
// hooks already configured for the same events.
func addStatusHooks(settings map[string]any) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, groups := range claudeStatusHooks() {
		existing, _ := hooks[event].([]any)
		for _, g := range groups {
			existing = append(existing, g)
		}
		hooks[event] = existing
	}
	settings["hooks"] = hooks
}
