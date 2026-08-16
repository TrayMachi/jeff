# Jeff deployment

Jeff uses Telegram `getUpdates` long polling. No public webhook endpoint or inbound firewall rule is required.

1. Create a bot with BotFather and copy its token into `/home/tray/project/jeff/.env`.
2. Put the permitted numeric Telegram chat IDs in `TELEGRAM_ALLOWED_CHAT_IDS`.
3. Copy and edit `config.example.yaml` as `config.yaml`; every project directory must be under `/home/tray/project`.
4. Build and install:

```bash
cd /home/tray/project/jeff
make check
sudo ./deploy/install.sh
```

Check logs with `journalctl -u jeff.service -f`. The service runs as `tray`, depends on `opencode.service`, restarts after failure, and stores its session database separately from Ivy.
