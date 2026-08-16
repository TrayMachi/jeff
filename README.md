# Jeff

Jeff is a Telegram-only OpenCode harness. It is a separate service from Ivy and does not modify Ivy's Lark bot.

## Setup

```bash
cd /home/tray/project/jeff
cp .env.example .env
cp config.example.yaml config.yaml
$EDITOR .env config.yaml
make check
```

Set `TELEGRAM_BOT_TOKEN` from BotFather. `TELEGRAM_ALLOWED_CHAT_IDS` is a comma-separated allowlist; an empty value rejects every chat. Keep each configured project explicitly listed under `projects` and keep its directory under `/home/tray/project`.

Jeff receives updates with Telegram long polling, so it does not need a public IP, webhook, tunnel, or listening port. In private chats it accepts ordinary messages. In groups it accepts messages mentioning the bot or supported commands.

## Commands

- `#alias prompt` selects a configured project for a new conversation.
- `/project alias prompt` does the same in a discoverable form.
- `/project` lists configured projects.
- `/status` shows the OpenCode session state.
- `/cancel` and `/stop` abort the active OpenCode turn.

A conversation stays bound to its first project. Start a new Telegram conversation to use another project.

OpenCode questions appear as inline keyboards. Single-select, multi-select, and custom text answers are supported. In group chats, only the user who started the turn can answer its questions.

## Service

Jeff depends on the existing local `opencode.service` and uses its own SQLite database at `data/jeff.sqlite`.

```bash
make build
sudo cp config.example.yaml config.yaml # edit before starting
sudo ./deploy/install.sh
journalctl -u jeff.service -f
```

The install script only manages `jeff.service`; it does not touch Ivy, `bot.service`, or `opencode.service`.
