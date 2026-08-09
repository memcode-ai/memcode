# Hooks

User-defined shell commands that run at agent lifecycle points — the extensibility seam
for policy and automation the prompt can't provide: deterministic guards, notifications,
formatters, context injection. Semantics deliberately match the de-facto hooks format used by mainstream agent CLIs, so
existing hook scripts port over.

## Configuration

Two plain-JSON files you own, merged in order (user first, then project):

| File | Scope |
| --- | --- |
| `~/.memcode/hooks.json` | every project |
| `<repo>/.memcode/hooks.json` | this project (runs after user hooks) |

Missing files are fine. Malformed JSON or a bad matcher becomes a one-time session
warning — a broken hooks.json never takes the agent down.

```json
{
  "hooks": {
    "pre_tool_use": [
      { "matcher": "bash", "command": "your-guard.sh", "timeout": 30 }
    ],
    "session_start": [
      { "command": "echo \"Deploy freeze until Friday — no gcloud run deploy.\"" }
    ]
  }
}
```

- `matcher` — a regexp **full-matched against the tool name** (`bash`, `edit_file`,
  `apply_patch`, `web_search`, …). Empty or omitted = every tool. Ignored for session
  events.
- `command` — run through the platform shell (`sh -c`; PowerShell on Windows), cwd =
  project root.
- `timeout` — seconds, default 60.

## Events

| Event | When | Semantics |
| --- | --- | --- |
| `session_start` | chat session opens | combined stdout is injected into the system prompt as standing context |
| `pre_tool_use` | before every tool call | **exit 2 blocks the call** — stderr becomes the reason the model sees; any other non-zero exit is a non-blocking warning |
| `post_tool_use` | after every executed tool call | advisory; exit codes surface as warnings |
| `session_end` | chat session closes | fire-and-forget |

A blocked call never executes; the model receives an error tool-result carrying your
stderr, so it adapts instead of stalling.

## Payload (stdin, JSON)

| Event | Fields |
| --- | --- |
| `session_start` / `session_end` | `{event, session_id, root}` |
| `pre_tool_use` | `{event, tool, input, session_id, root}` — `input` is the tool's raw JSON input, e.g. `{"command": "…"}` for bash |
| `post_tool_use` | pre fields plus `{result, is_error}` — `result` truncated at 16 KiB |

## Environment

Every hook runs with `MEMCODE_HOOK_EVENT`, `MEMCODE_TOOL_NAME` (empty for session
events), `MEMCODE_SESSION_ID`, and `MEMCODE_PROJECT_DIR` set, on top of your normal
environment.

## Examples

Block force-pushes no matter what the agent reasons itself into (`pre_tool_use`,
matcher `bash`):

```json
{ "matcher": "bash",
  "command": "jq -e '.input.command | test(\"push[^|;]*( --force|-f)\")' >/dev/null && { echo 'force-push is blocked by policy' >&2; exit 2; } || exit 0" }
```

gofmt every Go file the agent edits (`post_tool_use`, matcher `edit_file`):

```json
{ "matcher": "edit_file",
  "command": "f=$(jq -r .input.path); case \"$f\" in *.go) gofmt -w \"$f\";; esac" }
```
