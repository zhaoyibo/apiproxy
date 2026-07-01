# apiproxy

A lightweight self-hosted API proxy for Anthropic / OpenAI-compatible upstreams.
It multiplexes sub-keys on top of a real upstream key, enforces per-key CNY quotas,
records token usage per model per day, and serves a built-in admin UI.

## Features

- **Sub-key management** — issue isolated keys to users or apps, each with an optional CNY spending cap
- **Usage accounting** — records input / output / cache tokens and cost to MySQL (`daily_stats`)
- **Model price config** — per-model, context-tier pricing via the admin UI (or a screenshot OCR shortcut)
- **Session-based admin auth** — cookie + Redis session; brute-force lockout built in
- **Single binary** — Go backend + React frontend compiled into one statically linked binary via `embed.FS`
- **Docker Compose** ready

## Requirements

- Go 1.24+
- Node 22+ / pnpm
- MySQL 8+
- Redis 7+

## Quick start

```bash
cp .env.example .env   # fill in DSN, Redis URL, upstream URL, admin key
docker compose up -d
```

Or build from source:

```bash
# Frontend
cd web && pnpm install && pnpm build && cd ..

# Backend (embeds web/dist automatically)
go build -o apiproxy .
./apiproxy
```

The server listens on `PORT` (default `8080`).
Open `http://localhost:8080` for the admin UI.

## Environment variables

| Variable | Description |
|---|---|
| `PORT` | HTTP listen port (default `8080`) |
| `MYSQL_DSN` | MySQL data source name |
| `REDIS_ADDR` | Redis address (default `localhost:6379`) |
| `REDIS_PASSWORD` | Redis password (optional) |
| `PROXY_URL` | Upstream base URL, e.g. `https://api.anthropic.com` |
| `ADMIN_KEY` | Admin password for the management UI |
| `ALLOWED_ORIGINS` | Comma-separated extra CORS origins (optional) |
| `OCR_MODEL` | Model used for price-screenshot OCR (optional) |
| `OCR_API_KEY` | API key for the OCR model (optional) |

See [`.env.example`](.env.example) if present, or `internal/config/config.go` for full defaults.

## API usage

Proxy any Anthropic or OpenAI-compatible request through `/v1/*`:

```
POST /v1/messages          # Anthropic
POST /v1/chat/completions  # OpenAI
```

Authenticate with a sub-key via `Authorization: Bearer <key>` or `X-Api-Key: <key>`.

## Schema

See [`schema.sql`](schema.sql) — three tables: `api_keys`, `daily_stats`, `model_prices`.

## License

MIT
