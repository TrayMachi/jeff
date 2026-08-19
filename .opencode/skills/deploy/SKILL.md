---
name: deploy
description: Deploy Jeff by building its binary and restarting the existing systemd service. Use when deploying Jeff changes, running make deploy, or configuring passwordless deployment for this repository.
---

# Jeff Deploy

## Quick start

Run from the repository root:

```bash
make deploy
```

This builds `bin/jeff` and restarts `jeff.service`. It uses `sudo -n`, so it
fails immediately instead of prompting if the one-time sudoers setup has not
been completed.

## One-time setup

Install the service normally first, including the required `.env` and
`config.yaml` files:

```bash
sudo ./deploy/install.sh
```

Then validate and install the narrowly scoped sudoers rule:

```bash
sudo visudo -cf deploy/jeff-deploy.sudoers
sudo install -o root -g root -m 0440 deploy/jeff-deploy.sudoers /etc/sudoers.d/jeff-deploy
```

The rule allows only `systemctl restart jeff.service`. Do not run `sudo make
deploy`: the build should run as the normal user, and passwordless access must
not be granted to `make`, `install.sh`, or a shell.

## Unit changes

If `deploy/jeff.service` changes, install it manually with
`sudo ./deploy/install.sh` once. The regular `make deploy` path intentionally
only restarts the existing unit.
