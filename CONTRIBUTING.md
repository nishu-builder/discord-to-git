# Contributing

Keep the archive format stable, use Discord IDs in paths, and keep source code
separate from generated message data.

Use the pinned Nix environment:

```sh
nix develop
gofmt -w *.go
exit
nix flake check --print-build-logs
```

Tests use a fake Discord server and temporary Git repositories. They do not need
a bot token or access to a Discord server. Include regression tests for changes
to pagination, permissions, message reconciliation, and publishing.

Open a pull request describing the behavior change and how you checked it.
Use synthetic messages and IDs in tests and examples. Keep tokens, private
messages, local configuration, and deployment notes out of commits and issues.
