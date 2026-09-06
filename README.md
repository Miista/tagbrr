# tagbrr

Converges arr grab-time indexer flags onto qBittorrent tags.

Sonarr/Radarr know a release's promo status (freeleech, double upload, …)
only at grab time, and expose it only in the On Grab webhook — the data
never reaches the download client and is discarded after grab. tagbrr
catches that webhook and tags the torrent in qBittorrent, allowing for a
qui automation.

## Rules (`/config/tagbrr.yaml`)

Each key is a comma-separated list of flag patterns; any of them matching
(case-insensitive substring against the grab's `indexerFlags`) applies the tag:

```yaml
rules:
  doubleupload: du
  freeleech,halfleech: fl
```

## Environment

| Var | Default | Notes |
|---|---|---|
| `TAGBRR_QBIT_URL` | — (required) | e.g. `http://qbittorrent:8080` |
| `TAGBRR_QBIT_USER` | `admin` | |
| `TAGBRR_QBIT_PASS` | — (required) | |
| `TAGBRR_INTERVAL` | `2m` | reconcile interval |
| `TAGBRR_TTL` | `48h` | give up on a grabbed torrent that never appears in qBittorrent after this long; keep ≤ the add-to-removal lifetime of your torrents, longer buys nothing |
| `LOG_LEVEL` | `info` | zerolog level |
| `TZ` | UTC | timezone for log timestamps |

The rules file lives at `/config/tagbrr.yaml` and state at
`/data/watchlist.json` inside the container (fixed paths — mount
accordingly). The webhook listens on `:9171`.

## Compose

```yaml
tagbrr:
  image: ghcr.io/miista/tagbrr:latest
  container_name: tagbrr
  restart: unless-stopped
  environment:
    TAGBRR_QBIT_URL: http://qbittorrent:8080
    TAGBRR_QBIT_PASS: ${QBIT_PASS}
    TZ: Europe/Copenhagen
  volumes:
    - ./tagbrr/tagbrr.yaml:/config/tagbrr.yaml:ro   # rules file (fixed path)
    - ./tagbrr/data:/data                           # state (fixed path)
  networks: [media]   # arrs reach the webhook on :9171 over the compose network; nothing published
```

## Arr setup

Sonarr/Radarr → Settings → Connect → **Webhook**:
URL `http://tagbrr:9171/webhook`, method POST, trigger **On Grab** only.
The Test button sends `eventType: Test`, which tagbrr acks and ignores.

## Notes

- Flags only flow if the Prowlarr indexer definition parses promo data for
  the tracker (check a manual search in the arr for visible flags).
- The flag snapshot is grab-time only; promos ending later are invisible.
  Time-box downstream instead (qui rule on tag + added age).
- Failed grabs never appear in qBit; the TTL garbage-collects them.
