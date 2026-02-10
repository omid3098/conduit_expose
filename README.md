# conduit-expose

A lightweight monitoring agent for [Psiphon Conduit](https://github.com/Psiphon-Inc/conduit) servers. It auto-discovers running conduit containers, collects Docker stats and application metrics, and exposes everything through a single authenticated HTTP endpoint.

Built for operators running multiple conduit nodes with [conduit-manager](https://github.com/SamNet-dev/conduit-manager) who need centralized monitoring without SSH or root access.

## Quick Install

```bash
curl -sL https://raw.githubusercontent.com/omid3098/conduit_expose/main/install.sh | sudo bash
```

The installer will:
- Check for Docker (install it if missing)
- Ask you to pick an **expose port** (random high port suggested by default)
- Generate an **auth secret** (or let you set your own)
- Build and start the agent as a Docker container
- Print a **connection URI** you can paste straight into your monitoring dashboard

```
=====================================
   Installation complete!
=====================================

  Connection URI (paste into your monitoring dashboard):

  conduit://0267b7e7b00b59b99fa99095bf09ea6e@5.75.150.242:44626

  Manage: conduit-expose-ctl [status|restart|update|uninstall|show-config|uri]
```

> **Anti-censorship note:** The expose port is randomized by default (range 10000-65000) so that scanners cannot fingerprint conduit servers by probing a known port.

## How It Works

```
+-------------------+       +------------------+       +-------------------+
|  conduit-expose   | <---> |  Docker Daemon   | <---> |  conduit container|
|  (port: random)   |       |  /var/run/docker |       |  (port 9090)      |
+-------------------+       |  .sock           |       +-------------------+
        |                   +------------------+       +-------------------+
        |                           ^                  |  conduit container|
   GET /status                      |                  |  (port 9090)      |
   (auth required)                  +----------------> +-------------------+
```

1. **Discovery** - Finds all containers matching the `ghcr.io/psiphon-inc/conduit/cli` image or named `conduit*`
2. **Docker Stats** - Collects CPU%, memory usage, and uptime for each container
3. **App Metrics** - Queries each container's internal Prometheus endpoint (`<container-ip>:9090/metrics`) for connection and traffic data
4. **HTTP API** - Serves aggregated JSON on `GET /status`, protected by an auth header

## Connection URI

The installer outputs a single-line URI that encodes everything a monitoring dashboard needs:

```
conduit://SECRET@HOST:PORT
```

| Part | Maps to |
|---|---|
| `SECRET` | Value for the `X-Conduit-Auth` header |
| `HOST:PORT` | The HTTP endpoint (`GET http://HOST:PORT/status`) |

Retrieve it any time with:

```bash
conduit-expose-ctl uri          # raw URI, no colors (for scripts)
conduit-expose-ctl show-config  # full config including URI
```

## API

### `GET /status`

Requires header: `X-Conduit-Auth: <your-secret>`

```bash
curl -H "X-Conduit-Auth: your-secret" http://your-server:PORT/status
```

Response:

```json
{
  "server_id": "prod-node-07",
  "timestamp": 1739180400,
  "total_containers": 2,
  "containers": [
    {
      "id": "a1b2c3d4e5f6",
      "name": "conduit-1",
      "status": "running",
      "cpu_percent": 12.5,
      "memory_mb": 256.0,
      "uptime": "48h32m15s",
      "app_metrics": {
        "connections": 45,
        "traffic_in": 102400,
        "traffic_out": 204800
      }
    },
    {
      "id": "b2c3d4e5f6a7",
      "name": "conduit-2",
      "status": "running",
      "cpu_percent": 8.2,
      "memory_mb": 128.5,
      "uptime": "24h15m42s",
      "app_metrics": null
    }
  ]
}
```

`app_metrics` is `null` when the container's Prometheus endpoint is unreachable (e.g., container just started).

### `GET /health`

No authentication required. For load balancers and Docker health checks.

```bash
curl http://your-server:PORT/health
# {"status":"ok"}
```

## Management

After installation, use `conduit-expose-ctl` to manage the agent:

```bash
conduit-expose-ctl status       # Show running state, port, uptime
conduit-expose-ctl show-config  # Display config and connection URI
conduit-expose-ctl uri          # Print raw connection URI (for scripts)
conduit-expose-ctl restart      # Restart the container
conduit-expose-ctl update       # Pull latest source, rebuild, redeploy (also updates the ctl itself)
conduit-expose-ctl uninstall    # Remove everything
```

## Configuration

All settings are passed as environment variables to the container:

| Variable | Default | Description |
|---|---|---|
| `CONDUIT_AUTH_SECRET` | *(required)* | Token checked against `X-Conduit-Auth` header |
| `CONDUIT_LISTEN_ADDR` | `:8081` | Internal listen address (inside the container) |
| `CONDUIT_METRICS_PORT` | `9090` | Prometheus port inside conduit containers |
| `CONDUIT_METRICS_PATH` | `/metrics` | Prometheus endpoint path |
| `CONDUIT_POLL_INTERVAL` | `15s` | Data refresh interval |

The external port is set at install time via Docker's `-p <random>:8081` mapping and saved to `/etc/conduit-expose/config`.

## Manual Setup

If you prefer not to use the installer:

```bash
# Clone and build
git clone https://github.com/omid3098/conduit_expose.git
cd conduit_expose
docker build -t conduit-expose .

# Run (pick your own port and secret)
docker run -d \
  --name conduit-expose \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e CONDUIT_AUTH_SECRET=your-secret-here \
  -p 43721:8081 \
  conduit-expose
```

## Building from Source

Requires Go 1.23+:

```bash
git clone https://github.com/omid3098/conduit_expose.git
cd conduit_expose
go build -ldflags="-s -w" -o conduit-expose .
CONDUIT_AUTH_SECRET=your-secret ./conduit-expose
```

## License

MIT
