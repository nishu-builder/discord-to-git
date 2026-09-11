# discord-to-git

Turns selected Discord channels into ordinary Markdown files, commits changes, and optionally pushes them to a Git remote.

It uses Discord's official REST API and Go's standard library.

## What the archive looks like

```text
Team/
  general/
    CHANNEL.md
    2026-09-10.md
    threads/
      123456789012345678/
        THREAD.md
        2026-09-10.md
  random/
    CHANNEL.md
    2026-09-10.md
```

Daily files contain messages in chronological order, with author, UTC time,
message ID, reply links, edits, attachment links, and embed text. Thread IDs make
paths stable when thread titles change. CHANNEL.md lists the channel's threads.

You can use ordinary read, ls, and grep tools on a checkout of the archive.

## Start here if you are new to Go

Read the files in this order:

| File | Responsibility |
| --- | --- |
| main.go | Load configuration, run one sync, and repeat on a timer. |
| discord.go | Discord HTTP requests, permissions, pagination, and threads. |
| snapshot.go | Turn messages into the directory and Markdown layout. |
| git.go | Commit a completed snapshot and push it. |
| credential.go | Read a token from the environment or a private local socket. |

All files use package main, so they compile into one executable. A struct groups
related fields; a method such as api.messages is a function attached to a type.
Functions usually return a value and an error. Checking that error makes failure
paths explicit. Context carries cancellation and deadlines through network calls.

The *_test.go files use a local fake Discord server and real temporary Git repos.
They do not need credentials or contact Discord.

## Build and develop

Install Nix with flakes enabled, then:

```sh
nix develop
go test ./...
go run . -h

# Reproducible package, including its tests:
nix build
./result/bin/discord-to-git -h
```

flake.lock pins nixpkgs, including Go and Git. The development shell provides
Go, the Go language server (gopls), and Git. The packaged executable includes Git,
SSH, and a CA certificate path through its launcher.

Without Nix, Go 1.23+ and Git are enough:

```sh
go test ./...
go build -o bin/discord-to-git .
```

## Configure

Copy config.example.json to config.local.json. The example selects only
general and random in example server. Channel IDs select the source; folder names
choose paths in the output. Changing a Discord channel's display name does not
rename its configured folder.

- guild_id: Discord server ID.
- folder: top-level output folder, such as Team.
- channels: explicit channel IDs and unique folder names.
- branch: archive branch.
- remote: Git URL or absolute path; an empty string disables pushes.

Use a separate private Git repository for the archive. Do not point the archive
output at a code checkout: the program owns and replaces its generated files.
It refuses nonempty directories that it did not initialize.

## Run

Supply DISCORD_BOT_TOKEN through your credential provider, then:

```sh
# One complete snapshot:
nix run . -- -config config.local.json -out data/archive -once

# Keep refreshing, waiting five minutes after each completed attempt:
nix run . -- -config config.local.json -out data/archive -interval 5m
```

Only a bot token is accepted. The bot needs Message Content Intent, View Channel,
and Read Message History. It discovers active and archived threads. With Manage
Threads it can discover private archived threads; otherwise it falls back to the
private archived threads the bot has joined. This exports text/announcement
channels and their threads, not DMs or voice recordings.

The program makes only GET requests to Discord. It never posts messages or changes
permissions. A shared bot credential can still have broader permissions than this
program uses.

## How refresh works

1. Check permissions and discover threads under the selected channels.
2. Fetch every page of messages, respecting Discord rate limits.
3. Render a complete temporary directory.
4. Replace the generated checkout, commit if content changed, and push.

This intentionally rescans history each time. It keeps the code small and notices
old edits, removed messages, and deleted threads on the next successful refresh.
For very large servers, measure a full scan before adding incremental state.
Discord does not provide a transactional server-wide snapshot: changes made
during a scan can appear on the next one.

An API failure aborts the new snapshot, leaving the previous Git commit intact.
A failed push is retried on the next cycle even if there are no new messages.
The Git branch moves only after a complete fetch and render. Read a cloned
commit for a consistent view; the bot's working directory changes during publish.

Status is written beside the checkout as archive.status.json. It includes the last
attempt, duration, commit/message count on success, or the error on failure.
No-op refreshes do not create commits. Previous versions, including subsequently
deleted messages, remain in Git history.

Attachment names, sizes, URLs, and embed text are preserved. Binary media is not
downloaded; Discord attachment URLs may expire. Reactions, polls, and interactive
components are not fully represented in this first version.

## Running under a supervisor

Use a process manager such as systemd and set memory/CPU limits appropriate to
the archive. Run only one process per output directory; a process lock enforces
this.

For a broker that must keep tokens off disk, start with:

```sh
discord-to-git -config config.local.json -out data/archive \
  -token-socket /private/runtime/discord-token.sock
```

The process waits for one newline-terminated token on a Unix socket (mode 0600),
then closes and removes the socket. The token stays in process memory and is not
passed to Git. The socket's parent directory must belong to the service account
and be private. After a process restart, inject the token again. This is not a
persistent credential store or an automatic broker enrollment.

## Caos

The publisher advances the configured Git branch. At the start of a Caos run,
resolve that branch to a commit and use the pinned commit as the input. A normal
Git checkout exposes the Markdown files directly. Caos's remote :@@= locator
requires a full commit SHA; a moving branch name is not itself a cache key.

Source code and private message data belong in different repositories.
