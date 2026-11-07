![Cursor Local Bridge](https://github.com/user-attachments/assets/b1e3a49f-e02f-437c-9bf0-012320b2ccd4)

# Cursor Local Bridge

Cursor Local Bridge launches, watches, follows up on, and stops Cursor CLI agents on your own machine. Its command is `clb`. It gives each agent the lifecycle of a cloud agent: each run has an ID, a status, a queue of follow-up prompts, a git branch, and a webhook that fires when the run ends. A program that can run shell commands, such as an assistant connected over a remote shell, can drive local agents through `clb` the way it would drive cloud agents through an API.

Use it in environments where cloud agents are not available.

> Status: MVP phase 1. The end-to-end tests run against a fake agent that speaks the same protocol as the Cursor CLI. Some behavior of the real CLI with a logged-in account is not verified yet. See [Known gaps](#known-gaps).

## How it works

- `clb daemon` runs in the background and listens on a unix socket that only your user can open.
- `clb launch` creates a run. The daemon adds a git worktree for the run on a new branch `clb/<run id>`, starts `agent acp` in it, and sends your prompt over the [Agent Client Protocol](https://agentclientprotocol.com).
- The daemon stores every run and the events the agent streams in SQLite. [Events](#events) lists the types it skips. When a follow-up resumes a session, the history the agent replays is not stored again. The daemon also writes each run's events to an NDJSON file that you can `tail -f`.
- When the Cursor CLI sends a permission request, the run waits in `awaiting_permission` until you run `clb approve` or `clb deny`, and `clb` passes your answer back. Whether the CLI asks before it uses a tool depends on the CLI's own settings. It can apply a file edit without asking. See [Security](#security).
- When a run reaches `finished`, `failed`, or `stopped`, the daemon posts a signed webhook if you configured one.

`clb` uses the login of the Cursor CLI on your machine. It does not need or accept an API key.

## Requirements

- macOS or Linux
- git
- The [Cursor CLI](https://cursor.com/cli), logged in with `agent login`
- Go 1.27 or later, to build `clb`

## Quickstart

1. Install the Cursor CLI and log in:

   ```sh
   curl https://cursor.com/install -fsS | bash
   agent login
   ```

2. Install `clb`:

   ```sh
   go install github.com/nozomi-koborinai/cursor-local-bridge/cmd/clb@latest
   ```

3. Start the daemon in its own terminal. It logs to stderr and runs until you press Ctrl-C:

   ```sh
   clb daemon
   ```

   To start it at login on macOS, see [Run the daemon with launchd](#run-the-daemon-with-launchd).

4. In another terminal, launch a run from inside a git repository:

   ```sh
   cd ~/src/my-project
   clb launch "Add a --verbose flag to the CLI and document it in the README"
   ```

   `clb` prints the run as JSON. Copy its `id`, for example `run-3f9a2c7d1e0b`.

5. Watch the run:

   ```sh
   clb status run-3f9a2c7d1e0b
   clb logs --follow run-3f9a2c7d1e0b
   ```

6. If `status` is `awaiting_permission`, read `pendingPermission` and answer it:

   ```sh
   clb approve run-3f9a2c7d1e0b
   ```

7. When `status` is `finished`, read `summary`, then send a follow-up or stop the run:

   ```sh
   clb followup run-3f9a2c7d1e0b "Also add a test for the flag"
   clb stop run-3f9a2c7d1e0b
   ```

   `clb stop` removes the worktree. The branch `clb/run-3f9a2c7d1e0b` stays, so commits the agent made are kept. Uncommitted changes in the worktree are lost.

## Example: Grok Bot

In this example, Grok Bot drives Cursor Local Bridge for you. Grok Bot runs `clb` commands on your machine through a remote shell, and the daemon tells Grok Bot when a run ends by posting to a routine's webhook trigger. Any assistant that can run shell commands and receive a webhook can follow the same steps.

1. Keep `clb daemon` running on the machine. To start it at login on macOS, see [Run the daemon with launchd](#run-the-daemon-with-launchd).

2. In Grok Bot, create a routine with a webhook trigger. Put the trigger's URL and a long random secret in `~/.clb/config.json`, then restart the daemon:

   ```json
   {
     "webhook": {
       "url": "<the URL of the routine's webhook trigger>",
       "secret": "a long random string"
     }
   }
   ```

3. Ask Grok Bot for a change. Grok Bot launches a run and keeps the `id` from the JSON that `clb` prints:

   ```sh
   clb launch --repo ~/src/my-project "Fix the flaky date test in parser_test.go"
   ```

4. To follow the run, Grok Bot reads its status and its events:

   ```sh
   clb status run-3f9a2c7d1e0b
   clb logs run-3f9a2c7d1e0b
   ```

   `clb logs --follow` blocks until the run is no longer active. If the remote shell limits how long a command can run, poll `clb status` instead. If `status` is `awaiting_permission`, Grok Bot shows you `pendingPermission` and runs `clb approve` or `clb deny` with your answer.

5. To change course, Grok Bot sends a follow-up. To end the run, it stops the run:

   ```sh
   clb followup run-3f9a2c7d1e0b "Also cover dates before 1970"
   clb stop run-3f9a2c7d1e0b
   ```

6. When the run reaches `finished`, `failed`, or `stopped`, the daemon posts the signed payload to the webhook trigger, and the routine runs. The payload has the run's `id`, `status`, `branch`, and `summary`, so the routine can report the result or have Grok Bot send the next follow-up. If the receiver can compute HMAC-SHA256, have it check `X-Clb-Signature` before it acts on the payload. See [Webhook](#webhook).

The daemon sends the webhook from your machine, so the trigger's URL must be reachable from there.

## Run lifecycle

A run is in exactly one of these states.

| Status | Meaning |
| --- | --- |
| `queued` | The daemon is creating the worktree and starting the agent. |
| `running` | The agent is working on a prompt. |
| `awaiting_permission` | The agent sent a permission request for a tool call and waits until you approve or deny it. |
| `finished` | The agent ended its turn and no prompts are queued. The daemon closes the agent process, and a follow-up starts a new one. |
| `failed` | The agent process failed. `error` says why. |
| `stopped` | You stopped the run. Its worktree is removed. |

The commands change the state as follows.

| Command | Allowed in | Effect |
| --- | --- | --- |
| `launch` | | Creates a run in `queued`. |
| `followup` | `queued`, `running`, `awaiting_permission` | Queues the prompt. The agent gets it when its current turn ends. |
| `followup` | `finished` | Starts the agent again on the same session and sends the prompt. The run goes back to `queued`. |
| `approve`, `deny` | `awaiting_permission` | Answers the oldest pending permission request. |
| `stop` | any | Stops the agent, removes the worktree, and moves the run to `stopped`. A `failed` run stays `failed`. Stopping again only retries the worktree cleanup. |

A command in a state that does not allow it fails with a `conflict` error and changes nothing. The webhook fires each time a run enters `finished`, `failed`, or `stopped`.

If the daemon stops while runs are active, it marks those runs `failed` when it starts again.

## Commands

Every command except `daemon` and `logs` prints one JSON value on stdout. Errors go to stderr as `clb: <message>`, with exit code 1.

| Command | Prints |
| --- | --- |
| `clb daemon` | Runs the daemon in the foreground. |
| `clb launch [--repo DIR] [--ref REF] [--model MODEL] [--backend acp\|print] PROMPT` | The new run. `--repo` defaults to the current directory and `--ref` to `HEAD`. |
| `clb status ID` | The run. |
| `clb list` | An array of all runs, newest first. |
| `clb followup ID PROMPT` | The run. |
| `clb approve ID`, `clb deny ID` | The run. |
| `clb stop ID` | The run, after its worktree is removed. |
| `clb logs [--follow] ID` | The run's events, one JSON object per line. `--follow` waits for new events until the run is no longer active. |

A `PROMPT` of `-` reads the prompt from stdin. Every command accepts `--home DIR`. See [Configuration](#configuration).

An error message starts with a code that a script can match: `invalid`, `not_found`, `conflict`, or `internal`. For example, `clb approve` on a stopped run prints `clb: conflict: not allowed: cannot answer a permission for a stopped run`.

### Run object

```json
{
  "id": "run-3f9a2c7d1e0b",
  "status": "awaiting_permission",
  "repo": "/Users/you/src/my-project",
  "baseRef": "HEAD",
  "branch": "clb/run-3f9a2c7d1e0b",
  "worktree": "/Users/you/.clb/worktrees/run-3f9a2c7d1e0b",
  "backend": "acp",
  "sessionId": "5b0c1f9e-2a7d-4f3e-9c41-0d2e8a6b7c13",
  "queuedPrompts": 1,
  "pendingPermission": {
    "id": "perm-1",
    "title": "Run npm test",
    "toolCall": {
      "toolCallId": "call_1",
      "title": "Run npm test",
      "kind": "execute",
      "status": "pending",
      "rawInput": { "command": "npm test" }
    }
  },
  "createdAt": "2026-11-07T09:30:00.123456789Z",
  "updatedAt": "2026-11-07T09:30:04.987654321Z"
}
```

- Empty fields such as `summary`, `error`, and `model` are left out.
- `summary` is the agent's reply text from its most recent turn.
- `queuedPrompts` counts prompts that the agent has not received yet.
- `pendingPermission` is the oldest unanswered permission request. `toolCall` is the agent's description of the tool call, as sent by the Cursor CLI.

### Events

`clb logs` prints events like these:

```json
{"runId":"run-3f9a2c7d1e0b","seq":1,"time":"2026-11-07T09:30:00.2Z","type":"status","data":{"status":"queued"}}
{"runId":"run-3f9a2c7d1e0b","seq":4,"time":"2026-11-07T09:30:01.5Z","type":"agent_message_chunk","data":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"I'll add"}}}
```

- `status`, `prompt`, `permission_requested`, `permission_answered`, and `webhook` events come from the daemon.
- Other types come from the agent unchanged. With the `acp` backend they are ACP `sessionUpdate` values such as `agent_message_chunk`, `tool_call`, and `plan`, plus `turn_ended`. With the `print` backend they are stream-json `type` values.
- By default the daemon does not store `agent_thought_chunk` and `available_commands_update` events. The Cursor CLI sends them often, and in testing they made run logs large. To keep them, set `storeAllEvents` to `true` in the config file.
- The Cursor CLI sends the first `tool_call` event for a shell command with `rawInput` set to `{}` and puts the command in a later `tool_call_update` event with the same `toolCallId`. A `tool_call_update` carries only the fields that changed. To get a tool's input, read the latest `rawInput` among the events for its `toolCallId`.

The same lines are in `~/.clb/logs/<run id>.ndjson`. SQLite is the source of truth. The daemon rewrites the NDJSON files from SQLite when it starts.

## Webhook

To get a POST when a run ends, set `webhook.url` and `webhook.secret` in the config file. The body is:

```json
{
  "event": "statusChange",
  "timestamp": "2026-11-07T09:41:12.5Z",
  "id": "run-3f9a2c7d1e0b",
  "status": "finished",
  "repo": "/Users/you/src/my-project",
  "branch": "clb/run-3f9a2c7d1e0b",
  "summary": "Added --verbose and documented it."
}
```

`status` is `finished`, `failed`, or `stopped`. A `failed` payload has `error`.

The request has these headers:

| Header | Value |
| --- | --- |
| `X-Clb-Signature` | `sha256=` and the hex HMAC-SHA256 of the raw body, keyed with `webhook.secret` |
| `X-Clb-Event` | `statusChange` |
| `X-Clb-Delivery` | An ID that stays the same when a delivery is retried |

The daemon tries a delivery up to 3 times. It retries on network errors and 5xx responses.

To check a signature, compute the HMAC over the exact bytes you received, then compare in constant time:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(body)
want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
ok := hmac.Equal([]byte(want), []byte(r.Header.Get("X-Clb-Signature")))
```

## Configuration

`clb` keeps its files in `~/.clb`. Set `CLB_HOME` or pass `--home` to use another directory. The directory holds `config.json`, the database `clb.db`, the socket `clb.sock`, `logs/`, and `worktrees/`.

`config.json` is optional. Every field is optional:

```json
{
  "agentPath": "/Users/you/.local/bin/agent",
  "backend": "acp",
  "model": "",
  "socketPath": "",
  "worktreeDir": "",
  "storeAllEvents": false,
  "webhook": {
    "url": "https://example.com/hooks/clb",
    "secret": "a long random string"
  }
}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `agentPath` | `agent` | The Cursor CLI executable. |
| `backend` | `acp` | `acp` or `print`. `clb launch --backend` overrides it. |
| `model` | empty | Passed to the CLI as `--model`. Empty lets the CLI choose. `clb launch --model` overrides it. |
| `socketPath` | `<home>/clb.sock` | The daemon socket. It must be shorter than 104 bytes. |
| `worktreeDir` | `<home>/worktrees` | Where run worktrees go. |
| `storeAllEvents` | `false` | Store `agent_thought_chunk` and `available_commands_update` events too. |
| `webhook.url` | empty | Where to post run status changes. |
| `webhook.secret` | empty | The HMAC key. Required when `webhook.url` is set. |

Relative paths resolve against the home directory. Unknown fields are an error. The config file holds the webhook secret, so run `chmod 600 ~/.clb/config.json`.

## Backends

- `acp`, the default, runs `agent acp` and talks to it over the Agent Client Protocol on stdio. One agent process serves a run until the run finishes. The permission requests the CLI sends reach you through `awaiting_permission`.
- `print` runs `agent -p --output-format stream-json --resume <chat id>` once per prompt. It gets the chat ID from `agent create-chat`. Print mode cannot ask for permission, and `clb` never passes `--force`, so the CLI decides on its own which tools it may run. Use it only if `acp` does not work for you.

## Run the daemon with launchd

`packaging/launchd/io.github.nozomi-koborinai.clb.plist` starts `clb daemon` at login and restarts it if it exits. launchd does not expand `~`, so the file has placeholders.

1. Create the directory for the log file. launchd does not create it:

   ```sh
   mkdir -p -m 700 ~/.clb
   ```

2. Fill in the placeholders and install the file:

   ```sh
   sed -e "s|__CLB__|$(command -v clb)|" -e "s|__HOME__|$HOME|g" \
     packaging/launchd/io.github.nozomi-koborinai.clb.plist \
     > ~/Library/LaunchAgents/io.github.nozomi-koborinai.clb.plist
   ```

3. Load it:

   ```sh
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.nozomi-koborinai.clb.plist
   ```

The daemon logs to `~/.clb/daemon.log`. The plist puts `~/.local/bin`, where the Cursor CLI installer puts `agent`, on `PATH`. If `agent` is somewhere else, set `agentPath` in `config.json`.

## Security

- The socket is mode 0600, and the daemon checks that each connecting process runs as the same user. `clb` creates `~/.clb` and its subdirectories with mode 0700 and the database and logs with mode 0600. It does not change the mode of a directory that already exists.
- `clb` forwards each permission request that the Cursor CLI sends and answers it only when you run `clb approve` or `clb deny`. It never passes `--force` or `--yolo` to the CLI.
- Whether the CLI sends a request before it uses a tool depends on the CLI's own settings, not on `clb`. In testing with a logged-in CLI, the agent applied a file edit without sending a request. To block a tool or a path outright, add it to `permissions.deny` in `~/.cursor/cli-config.json` or the project's `.cursor/cli.json`. See [Permissions](https://cursor.com/docs/cli/reference/permissions) in the Cursor CLI docs.
- `approve` and `deny` answer only the request in front of you. `clb` picks the agent's "allow once" or "reject once" option and never an "always" option. If the agent offers no such option, `clb` answers `cancelled`.
- An agent runs with your user's rights in its worktree. Review the run's branch before you merge it.

## Known gaps

A run against a logged-in Cursor CLI streamed events and applied a file edit. These parts are tested against the fake agent only:

- Permission requests from the real CLI, and the options they offer.
- Resuming a session with `session/load` after a run finishes.
- Whether `--model` changes the model in `agent acp`.
- The exact stream-json output of `agent -p`.

A webhook that is still being delivered when the daemon stops is not retried after a restart.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
