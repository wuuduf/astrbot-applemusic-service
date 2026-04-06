# AstrBot Service Mode

This project includes an AstrBot-oriented local HTTP API mode, designed for QQ NapCat/OneBot plugins.

## Start

1. Prepare config:

```bash
cp config.example.yaml config.yaml
# edit config.yaml with your Apple account related settings
```

2. Run API mode:

```bash
go run . --astrbot-api --astrbot-api-listen 127.0.0.1:27198
```

Optional auth token (recommended when not binding loopback):

```bash
export ASTRBOT_API_TOKEN='your-strong-token'
```

Health check:

```bash
curl http://127.0.0.1:27198/healthz
```

## API

- `POST /v1/search`
- `POST /v1/resolve-url`
- `POST /v1/artist-children`
- `POST /v1/download`
- `GET /v1/jobs/{job_id}`
- `POST /v1/artwork`
- `POST /v1/lyrics`

## Notes

- Downloads are processed by a background worker queue (`queued/running/completed/failed`).
- Do not commit `config.yaml` or runtime download/cache directories.
- If `ASTRBOT_API_TOKEN` is set, all `/v1/*` endpoints require `Authorization: Bearer <token>` (or `X-AstrBot-Token`).
