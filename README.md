![Cursor Local Bridge](https://github.com/user-attachments/assets/b1e3a49f-e02f-437c-9bf0-012320b2ccd4)

# Cursor Local Bridge

Cursor Local Bridge launches, watches, follows up on, and stops Cursor CLI agents on your own machine. Its command is `clb`. It gives each agent the lifecycle of a cloud agent: each run has an ID, a status, a queue of follow-up prompts, a git branch, and a webhook that fires when the run ends. A program that can run shell commands, such as an assistant connected over a remote shell, can drive local agents through `clb` the way it would drive cloud agents through an API.

Use it in environments where cloud agents are not available.

> Status: MVP phase 1. The end-to-end tests run against a fake agent that speaks the same protocol as the Cursor CLI. Manual tests against the real CLI with a logged-in account covered the main `acp` flows. See [Known gaps](#known-gaps).

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
| `failed` | The agent process failed. `error` says why. The worktree stays until you run `clb stop`. |
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

When the daemon closes an agent process, it also ends the processes that the agent started, such as a dev server or a command that it ran in the background. It finds them in two ways:

- It adds a `CLB_PROC_TOKEN` variable to the agent's environment and ends the processes that inherited it. With the `print` backend, this happens after each prompt.
- It ends your processes whose working directory is inside the run's worktree. The Cursor CLI starts terminal commands with an empty environment, so the token does not reach them. This happens after the agent process closes, when the run finishes, fails, or stops, or when the daemon shuts down. It happens again just before `clb stop` removes the worktree.

The second way also ends a process of yours that is not the agent's, such as a shell that you opened in the worktree. `clb` sends SIGTERM, then SIGKILL to any that still run 1 second later. It never matches a working directory outside the run's worktree.

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
    "title": "`npm test`",
    "toolCall": {
      "toolCallId": "call_1",
      "title": "`npm test`",
      "kind": "execute",
      "status": "pending",
      "rawInput": { "command": "npm test" },
      "content": [
        { "type": "content", "content": { "type": "text", "text": "Not in allowlist: npm" } }
      ]
    },
    "options": [
      { "optionId": "allow-once", "name": "Allow once", "kind": "allow_once" },
      { "optionId": "allow-always", "name": "Always allow", "kind": "allow_always" },
      { "optionId": "reject-once", "name": "Reject", "kind": "reject_once" }
    ]
  },
  "createdAt": "2026-11-07T09:30:00.123456789Z",
  "updatedAt": "2026-11-07T09:30:04.987654321Z"
}
```

- Empty fields such as `summary`, `error`, and `model` are left out.
- `summary` is the agent's reply text from its most recent turn. When tool calls split the reply into several blocks of text, `summary` joins the blocks with a newline.
- `error` says why the run failed. When the agent process exited, it includes the first 512 bytes of the agent's stderr and how many bytes it left out. The webhook payload has the same text.
- `queuedPrompts` counts prompts that the agent has not received yet.
- `pendingPermission` is the oldest unanswered permission request. `toolCall` is the agent's description of the tool call, as sent by the Cursor CLI. For a shell command the Cursor CLI puts the command in `title` and the reason it asks, such as `Not in allowlist: npm`, in `content`.
- `options` lists the choices the agent offered, as it sent them. `clb approve` picks the option whose `kind` is `allow_once`, and `clb deny` the one whose `kind` is `reject_once`. The `permission_requested` event carries the same `options`.

### Events

`clb logs` prints events like these:

```json
{"runId":"run-3f9a2c7d1e0b","seq":1,"time":"2026-11-07T09:30:00.2Z","type":"status","data":{"status":"queued"}}
{"runId":"run-3f9a2c7d1e0b","seq":4,"time":"2026-11-07T09:30:01.5Z","type":"agent_message_chunk","data":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"I'll add"}}}
```

- `status`, `prompt`, `permission_requested`, `permission_answered`, and `webhook` events come from the daemon.
- Other types come from the agent unchanged. With the `acp` backend they are ACP `sessionUpdate` values such as `agent_message_chunk`, `tool_call`, and `plan`, plus `turn_ended`. With the `print` backend they are stream-json `type` values.
- `turn_ended` marks the end of a turn that the agent finished. A run that you stop during a turn has no `turn_ended` event for that turn.
- By default the daemon does not store `agent_thought_chunk` and `available_commands_update` events, or the `thinking` events of the `print` backend. The Cursor CLI sends them often, and in testing they made run logs large. To keep them, set `storeAllEvents` to `true` in the config file.
- The Cursor CLI sends the first `tool_call` event for a shell command with `rawInput` set to `{}` and puts the command in a later `tool_call_update` event with the same `toolCallId`. A `tool_call_update` carries only the fields that changed. To get a tool's input, read the latest `rawInput` among the events for its `toolCallId`.
- After you deny a permission request, the Cursor CLI does not run the command, but a later `tool_call_update` for that `toolCallId` still reports `status` as `completed`. To tell whether a command ran, find the `permission_requested` event whose `toolCall.toolCallId` matches, then read `approved` in the `permission_answered` event with the same `id`.

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
  "trustWorktree": false,
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
| `storeAllEvents` | `false` | Store `agent_thought_chunk`, `available_commands_update`, and `thinking` events too. |
| `trustWorktree` | `false` | Pass `--trust` to `agent -p`. The `print` backend needs it. See [Backends](#backends). |
| `webhook.url` | empty | Where to post run status changes. |
| `webhook.secret` | empty | The HMAC key. Required when `webhook.url` is set. |

Relative paths resolve against the home directory. Unknown fields are an error. The config file holds the webhook secret, so run `chmod 600 ~/.clb/config.json`.

## Backends

- `acp`, the default, runs `agent acp` and talks to it over the Agent Client Protocol on stdio. One agent process serves a run until the run finishes. The permission requests the CLI sends reach you through `awaiting_permission`.
- `print` runs `agent -p [--trust] --output-format stream-json --resume <chat id>` once per prompt, with `--trust` only when `trustWorktree` is `true`. It gets the chat ID from `agent create-chat`. Print mode cannot ask for permission, and `clb` never passes `--force`, so the CLI decides on its own which tools it may run. Use it only if `acp` does not work for you.

`agent -p` refuses to run in a workspace that you have not trusted, and every run gets a new worktree. To use the `print` backend, set `trustWorktree` to `true` in the config file, and `clb` passes `--trust`. `--trust` trusts the workspace without a prompt. Unlike `--force`, it does not allow commands. Without `trustWorktree`, `clb launch --backend print` and `clb followup` on a print run fail with an `invalid` error that says how to turn it on.

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
- `clb` forwards each permission request that the Cursor CLI sends and answers it only when you run `clb approve` or `clb deny`. It never passes `--force` or `--yolo` to the CLI. It passes `--trust` to `agent -p` only when `trustWorktree` is `true`.
- Whether the CLI sends a request before it uses a tool depends on the CLI's own settings, not on `clb`. In testing with a logged-in CLI, the agent applied a file edit without sending a request. To block a tool or a path outright, add it to `permissions.deny` in `~/.cursor/cli-config.json` or the project's `.cursor/cli.json`. See [Permissions](https://cursor.com/docs/cli/reference/permissions) in the Cursor CLI docs.
- `approve` and `deny` answer only the request in front of you. `clb` picks the agent's "allow once" or "reject once" option and never an "always" option. If the agent offers no such option, `clb` answers `cancelled`.
- An agent runs with your user's rights in its worktree. Review the run's branch before you merge it.
- When a run's agent closes, `clb` ends every process of yours whose working directory is inside that run's worktree, including a shell that you opened there. See [Run lifecycle](#run-lifecycle).

## Known gaps

Manual tests against Cursor CLI 2026.09.18 with a logged-in account covered these parts of the `acp` backend: streamed events, a file edit, an approved and a denied permission request, a follow-up that resumes the session with `session/load`, `--model`, the error for an unknown model, the newlines in `summary`, and a stop during a turn. They also covered the `print` backend with `trustWorktree`, for a launch and a follow-up. These parts are tested against the fake agent only:

- Ending the background commands of the real Cursor CLI by their working directory. The fake agent starts its background command the way the real CLI does, with an empty environment, in its own session, and in the worktree. The macOS code that reads another process's working directory runs in the macOS CI job.

Other gaps:

- A command that the agent starts outside the worktree with an empty environment keeps running after the run ends.

- The Cursor CLI creates a folder in `~/.cursor/projects` for each run's worktree, named after the worktree path, such as `~/.cursor/projects/<path>-worktrees-run-3f9a2c7d1e0b`. `clb` does not remove these folders.
- When `agent -p --resume` gets a chat ID that the CLI does not know, it starts a new chat and reports no error. A follow-up on a `print` run then reaches an agent without the earlier conversation, and `clb` cannot tell.
- A webhook that is still being delivered when the daemon stops is not retried after a restart.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
