<img align="right" width="110" src="https://upload.wikimedia.org/wikipedia/en/3/30/EDIinMassEffect3.png" alt="EDI, the synthetic squadmate from Mass Effect 3">

# EDI

> *“I always work at optimal capacity.”*  
> — [**EDI**](https://en.wikipedia.org/wiki/EDI_(Mass_Effect))

EDI is a personal Telegram bot. At the moment, it relays service alerts for London Underground, Elizabeth line, DLR, and London Overground during chosen hours.

## Quickstart

1. Ensure [Docker Compose](https://docs.docker.com/compose/) is installed.
2. Get a bot token from [@BotFather.](https://t.me/BotFather)
3. Get your numeric Telegram user ID.
4. Create a data volume for EDI with owner `65532:65532` (the non-root container user) and `u=rwx,g=,o=` permissions.

Then, run:

```shell
export EDI_TELEGRAM_TOKEN="your-bot-token"
export EDI_TELEGRAM_ALLOWED_CHATS="user-id,another-user-id"
export EDI_DATA_VOLUME_PATH="data-volume-path-here"

docker compose up -d
```

Start a chat with the bot and use the inline menu to configure its behaviour.

## Configuration

Only the `token` and `allowed_chats` settings are required. For other settings and their default
values, refer to the complete `config.yaml` example below.

> [!NOTE]
> `EDI_*` environment variables override configuration file values
> (for example, `EDI_TELEGRAM_TOKEN`).

```yaml
telegram:
  # Required; get it from @BotFather.
  token: "your-bot-token"
  # Required; a list of positive Telegram private chat IDs (the same as user IDs).
  allowed_chats:
    - 123456789

tfl:
  # Optional; TfL API key.
  app_key: ""
  monitor:
    # Time between polling TfL API; minimum 30s.
    poll_interval: 1m
    # TfL API HTTP request timeout; must be positive.
    http_timeout: 10s
    # Consecutive TfL API failures before reporting unavailability.
    failure_threshold: 3
    # IANA timezone name.
    default_timezone: Europe/London
  notifications:
    # Good observations must span this period before announcing recovery.
    recovery_confirmation_period: 3m

# Optional; override the OS-specific persistence directory.
# Defaults to `$XDG_DATA_HOME/edi`, or `~/.local/share/edi` on Linux,
# and `~/Library/Application Support/edi` on macOS.
# data_directory: /path/to/edi

logging:
  # `debug`, `info`, `warn`, or `error`; -log-level overrides this.
  level: info
  # `json` or `text`.
  format: text
```

Keep this file private. Load it explicitly with `-config`; an unreadable or invalid file causes startup to fail. Restart the bot to pick up configuration changes.

## Development

Install the Go version in [go.mod](go.mod) and `golangci-lint`, then run the bot locally:

```sh
export EDI_TELEGRAM_TOKEN="your-bot-token"
export EDI_TELEGRAM_ALLOWED_CHATS="user-id,another-user-id"

# Run all the checks
make check

# Run the tests
make test

# Check concurrent code
make test-race

# Build the binary
make build

# Run the bot
make run

# Build a container image
make container
```

---

<sub>EDI character artwork © Electronic Arts / BioWare, via <a href="https://en.wikipedia.org/wiki/File:EDIinMassEffect3.png">Wikipedia</a>. This project is unaffiliated with Mass Effect or Transport for London.</sub>
