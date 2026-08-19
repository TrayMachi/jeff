# Jeff Agent Guide

## Project

Jeff is a Telegram-only OpenCode harness. It polls Telegram updates, streams
OpenCode turns, persists conversation state in SQLite, and runs as the
`jeff.service` systemd unit.

## Layout

- `cmd/jeff`: application entrypoint and dependency wiring.
- `internal/app`: application lifecycle and orchestration.
- `internal/telegram`: Telegram API client, polling, routing, and models.
- `internal/opencode`: OpenCode client, event handling, and turn broker.
- `internal/conversation`, `internal/contexts`, `internal/session`, `internal/store`:
  persisted conversation and session state.
- `internal/events`, `internal/question`, `internal/streamer`: event dispatch,
  OpenCode questions, and Telegram response streaming.
- `internal/config` and `internal/projects`: configuration and configured project
  resolution.
- `deploy`: systemd installation and deployment assets.

## Development

- Use Go 1.25 as declared in `go.mod`.
- Format modified Go files with `gofmt`.
- Add or update focused `*_test.go` tests with behavior changes.
- Run `make check` before submitting changes. It formats the tree, then runs
  `go test -race ./...`, `go vet ./...`, and a production build.

## Boundaries

- Keep Telegram-specific behavior in `internal/telegram`; do not leak API models
  into unrelated packages.
- Keep OpenCode protocol and event details in `internal/opencode`.
- Preserve the conversation-to-project binding: a conversation remains attached
  to the project selected by its first message.
- Treat configuration, environment variables, bot tokens, allowlists, and SQLite
  data as sensitive. Never commit `.env`, `config.yaml`, or files under `data/`.
- `config.example.yaml` is the safe template for configuration changes.

## Deployment

- `make deploy` builds the binary and restarts only `jeff.service` through the
  narrowly scoped sudoers rule.
- Changes to `deploy/jeff.service` or other installation assets require
  `sudo ./deploy/install.sh`; do not assume `make deploy` installs unit changes.
- Jeff is separate from Ivy. Do not modify Ivy services, configuration, or data
  while working on this repository.
