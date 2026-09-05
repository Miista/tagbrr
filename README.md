# tagbrr

Converges arr grab-time indexer flags onto qBittorrent tags.

Sonarr/Radarr know a release's promo status (freeleech, double upload, …)
only at grab time, and expose it only in the On Grab webhook — the data
never reaches the download client and is discarded after grab. tagbrr
catches that webhook, puts the torrent on a persistent watch list, and a
reconcile loop tags it in qBittorrent once it appears. Seeding *policy*
stays downstream — e.g. a qui automation: `tag du + added age < 30d →
seeding time unlimited`.

## Rules (`/config/tagbrr.yaml`)

Each key is a comma-separated list of flag patterns; any of them matching
(case-insensitive substring against the grab's `indexerFlags`) applies the tag:

```yaml
rules:
  doubleupload: du
  freeleech,halfleech: fl
```

## Environment

| Var | Default | |
|---|---|---|
| `TAGBRR_QBIT_URL` | — (required) | e.g. `http://qbittorrent:8080` |
| `TAGBRR_QBIT_USER` | `admin` | |
| `TAGBRR_QBIT_PASS` | — (required) | |
| `TAGBRR_LISTEN` | `:9171` | webhook listen address |
| `TAGBRR_INTERVAL` | `2m` | reconcile interval |
| `TAGBRR_TTL` | `48h` | drop watch entries that never appear in qBit; keep ≤ the add-to-removal lifetime of your torrents, longer buys nothing |
| `TAGBRR_CONFIG` | `/config/tagbrr.yaml` | |
| `TAGBRR_DATA` | `/data/watchlist.json` | |

## Compose

```yaml
tagbrr:
  image: ghcr.io/miista/tagbrr:latest
  container_name: tagbrr
  restart: unless-stopped
  environment:
    TAGBRR_QBIT_URL: http://qbittorrent:8080
    TAGBRR_QBIT_PASS: ${QBIT_PASS}
  volumes:
    - ./tagbrr/tagbrr.yaml:/config/tagbrr.yaml:ro
    - ./tagbrr/data:/data
  networks: [media]   # no published ports; arrs reach it on the compose network
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
