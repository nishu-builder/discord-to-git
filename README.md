# discord-to-git

Turns selected Discord channels into one JSON file per message, commits changes, and optionally pushes them to a Git remote.

It uses Discord's official REST API and Go's standard library.

## What the archive looks like

```text
data.json                              # schema version and guild ID
channels/
  <channel-id>/
    data.json                          # channel name, topic, parent ID, etc.
    messages/
      <message-id>/
        data.json                      # message and optional thread metadata
        <reply-message-id>/
          data.json                    # a message inside that thread
users/
  <user-id>/
    data.json                          # username, global name, avatar, bot flag
```

Paths use Discord snowflakes (stable IDs), never names or timestamps.
The spelling and capitalization of `channels`, `messages`, and `users` are
part of this format. Each ID is a JSON string, preserving its full precision.

Message files contain `author_id`, `mention_ids`, and reaction
`normal_user_ids` / `burst_user_ids`. These refer to
`users/<id>/data.json`. Usernames are stored only in that registry.
Content retains Discord's original mention syntax, such as `<@123>`.
A username change updates its user record without rewriting message bodies.

Messages also include their ID, Discord channel ID, original timestamp, edited
timestamp, content, type, flags, pinned status, attachment and embed metadata,
and message references. Use `timestamp` to order messages; IDs can also be
sorted numerically without converting them to floating point.

A thread's metadata lives in the starter's `data.json` under `thread`,
including its name, ID, owner ID, archive status, and parent channel ID.
Replies are direct child directories of the starter, without another messages
or threads directory. Replies to individual messages inside a thread remain
siblings; `message_reference` records that relationship.

Discord gives a thread created from a message the same ID as its starter.
If the starter was deleted, or the thread was created without a parent message,
its directory has a placeholder `data.json` containing `id`, `channel_id`,
`missing: true`, and `thread`. It has no invented author, content, or timestamp.
If a real starter with that ID appears inside the thread itself, it occupies
the root record. Threads are never duplicated as top-level channel folders.

Message `channel_id` and `message_reference.channel_id` retain Discord's IDs:
inside a thread they identify the thread, not the top-level archive channel.
A reference may point to a deleted message or something outside the selected
channels. References do not trigger an expansion of the archive's channel scope.

The user registry contains authors, mentioned users, reactors, and thread owners
encountered in this snapshot. It is not a guild membership directory. Profile
values are those returned during the scan; nickname history is not reconstructed.
Discord can also supply webhook-shaped authors; their returned ID and profile
are preserved, with `webhook_id` identifying webhook messages.

JSON is indented for ordinary read and grep tools. JSON strings escape embedded
newlines; decode the content field when displaying multi-line messages.
The root README links channel IDs to their names for browsing.

Discord API references: [threads](https://docs.discord.com/developers/topics/threads)
and [messages and reactions](https://docs.discord.com/developers/resources/message).

## Build and develop

Use Nix with flakes enabled:

```sh
nix build
nix flake check
nix run . -- -h
```

flake.lock pins the toolchain and dependencies. The package includes Git, SSH,
and CA certificates. The build runs the tests against a local fake Discord server
and temporary Git repositories; no Discord credential is needed.

For development, enter the Nix shell:

```sh
nix develop
```

The shell provides Go, gopls, and Git.

## Configure

Copy config.example.json to config.local.json. The example selects only
general and random in example server. Channel IDs select both the source
and the output paths. Renaming a channel changes its metadata only.

- guild_id: Discord server ID.
- channels: a list of objects, each with an explicit channel id.
- branch: archive branch.
- remote: Git URL, absolute path, or configured Git remote name; an empty string disables pushes.

Use a separate private Git repository for the archive. Do not point the archive
output at a code checkout: the program owns and replaces its generated files.
It refuses nonempty directories that it did not initialize.

## Run

Supply DISCORD_BOT_TOKEN through your credential provider, then:

```sh
# One incremental refresh (backfills an empty archive):
nix run . -- -config config.local.json -out data/archive -once

# Force a complete reconciliation of history:
nix run . -- -config config.local.json -out data/archive -once -full

# Keep syncing, pausing one minute after each completed attempt:
nix run . -- -config config.local.json -out data/archive
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

1. Check permissions and discover active and archived threads.
2. For each channel or thread, fetch messages newer than its saved position and
   recheck messages created within the last 24 hours.
3. Retain older JSON records, replace the fetched range, and render a complete
   temporary snapshot. Missing messages in that range become deletions.
4. Commit only changed content and push the configured branch.

An interrupted publish is recovered from Git HEAD before the next scan; uncommitted
files in the bot-owned output directory are discarded. The committed archive is the checkpoint: the greatest message ID in each channel
or thread records progress. No database is required. After an outage, fetching
continues from that checkpoint even when it is older than the overlap window.
Newly added channels and newly discovered threads receive a full backfill.

Recent messages are fetched with their reaction users even when reaction counts
are unchanged. Edits, deletions, reaction changes, and profile changes affecting
older messages are reconciled by a full scan every 24 hours. Thread discovery
runs on every cycle, including archived threads; it does not skip a thread just
because its archived flag is unchanged. This is polling, not a Gateway event
listener, so changes between polls can be missed.

Defaults and overrides:

| Flag | Default | Meaning |
| --- | --- | --- |
| -interval | 1m | Pause after each completed attempt |
| -lookback | 24h | Creation-time window to recheck for message changes |
| -full-interval | 24h | Time between full history reconciliations |
| -sync-timeout | 2h | Limit for one attempt, including backfill |
| -full | false | Fully reconcile on every attempt |

A full scan can take much longer than an incremental scan; it runs in place of
that cycle's incremental refresh. Schedule state lives beside the checkout in
archive.sync-state.json and survives restarts. On upgrade, an existing archive
is reused immediately and its first scheduled full reconciliation is due one
day later. A missing archive or stream is always backfilled.

Discord does not provide a transactional server-wide snapshot: changes made
during a scan can appear on the next one. See the
[message pagination API](https://docs.discord.com/developers/resources/message#get-channel-messages).

An API failure aborts the new snapshot, leaving the previous Git commit intact.
A failed push is retried on the next cycle even if there are no new messages.
The Git branch moves only after a complete fetch and render. Read a cloned
commit for a consistent view; the bot's working directory changes during publish.

Status is written beside the checkout as archive.status.json. It includes the last
attempt, scan mode, duration, next full reconciliation and commit/message count
on success, or the error on failure.
No-op refreshes do not create commits. Previous versions, including subsequently
deleted messages, remain in Git history.

Attachment and embed JSON is preserved. Binary media is not downloaded; Discord
attachment URLs may expire. Normal and burst reaction users are fetched separately;
reported counts can briefly differ from user lists if a reaction changes during a
scan. Polls, interactive components, forwarded snapshots, and interaction metadata
are not represented in this version. Fetching reactors adds API requests, so a full
scan takes longer than the previous text-only export.

## Running under a supervisor

Use a process manager such as systemd and set memory/CPU limits appropriate to
the archive. Run only one process per output directory; a process lock enforces
this.

For a broker that must keep tokens off disk, start with:

```sh
nix run . -- -config config.local.json -out data/archive \
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
Git checkout exposes the JSON files directly. Caos's remote :@@= locator
requires a full commit SHA; a moving branch name is not itself a cache key.

Source code and private message data belong in different repositories.
