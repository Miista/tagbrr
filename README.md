# tagbrr

Converges arr grab-time indexer flags onto qBittorrent tags.

Sonarr/Radarr know a release's promo status (freeleech, double upload, …)
at grab time and keep it on the grab event in their history — the data
never reaches the download client. tagbrr polls that history and tags the
torrent in qBittorrent, allowing for a qui automation. Nothing is
configured in the arrs: tagbrr only reads.

## Config (`/config/tagbrr.yaml`)

Rules only. Keys are comma-separated flag patterns; any of them matching
(case-insensitive substring against the grab's indexer flags) applies the
tag:

```yaml
rules:
  doubleupload: du
  freeleech,halfleech: fl
```

The arrs to poll are declared entirely in the environment: a
`TAGBRR_ARR_<NAME>_URL` per instance, each with a matching
`TAGBRR_ARR_<NAME>_KEY`.

## Environment

| Var | Default | Notes |
|---|---|---|
| `TAGBRR_QBIT_URL` | — (required) | e.g. `http://qbittorrent:8080` |
| `TAGBRR_QBIT_USER` | `admin` | |
| `TAGBRR_QBIT_PASS` | — (required) | |
| `TAGBRR_ARR_<NAME>_URL` | — (required, per arr) | e.g. `TAGBRR_ARR_RADARR_URL=http://radarr:7878` |
| `TAGBRR_ARR_<NAME>_KEY` | — (required, per arr) | API key matching the `_URL` of the same name |
| `TAGBRR_INTERVAL` | `15m` | poll interval; keep it well under your shortest seeding goal so tags land before policy would matter |
| `TAGBRR_WINDOW` | `30d` | how far back each poll looks; grabs older than this are never (re)considered. Set it long once to retroactively tag old grabs still in qBittorrent |
| `LOG_LEVEL` | `info` | zerolog level |
| `TZ` | UTC | timezone for log timestamps |

The config file lives at `/config/tagbrr.yaml` inside the container (fixed
path — mount accordingly). tagbrr is stateless: no volume, no database;
every pass re-reads the window and tags only what is missing a tag.
`:9171` serves `/healthz` only. Durations accept Go forms plus `d`/`w`
suffixes (`12h`, `1d`, `2w`).

## Compose

```yaml
tagbrr:
  image: ghcr.io/miista/tagbrr:latest
  container_name: tagbrr
  restart: unless-stopped
  environment:
    TAGBRR_QBIT_URL: http://qbittorrent:8080
    TAGBRR_QBIT_PASS: ${QBIT_PASS}
    TAGBRR_ARR_RADARR_URL: http://radarr:7878
    TAGBRR_ARR_RADARR_KEY: ${RADARR_API_KEY}
    TAGBRR_ARR_SONARR_URL: http://sonarr:8989
    TAGBRR_ARR_SONARR_KEY: ${SONARR_API_KEY}
    TZ: Europe/Copenhagen
  volumes:
    - ./tagbrr/tagbrr.yaml:/config/tagbrr.yaml:ro   # config (fixed path)
  networks: [media]   # polls the arrs and qBittorrent over the compose network; nothing published
```

## Notes

- Flags only flow if the Prowlarr indexer definition parses promo data for
  the tracker (check a manual search in the arr for visible flags).
- The flag snapshot is grab-time only; promos ending later are invisible.
  Time-box downstream instead (qui rule on tag + added age).
- Failed grabs never appear in qBit; they simply age out of the window.
- Statelessness means restarts and downtime lose nothing — history is the
  durable record — and a tag removed by hand is restored on the next pass
  while the grab is inside the window.
