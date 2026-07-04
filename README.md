# avd-farm

On-demand, **ephemeral Android virtual devices** exposed over ADB, with a tiny HTTP
control plane and a companion CLI. Ask for a device, get back an ADB endpoint,
use it for the length of a session, and have it torn down automatically when
you're done.

`avd-farm` is designed for **agents and automation**: a coding agent (or a human)
requests a fresh, Play-Store-ready emulator, installs an APK, drives it over ADB
or `uiautomator`, optionally watches the screen over a web viewer, and releases
it. Every device is a clean, disposable clone of a golden snapshot — nothing
persists between sessions.

---

## What's in this repo

| Path            | What it is                                                              |
| --------------- | ----------------------------------------------------------------------- |
| `cmd/avdd`      | **The server** (`avdd`) — the HTTP control plane + device lifecycle manager. |
| `cmd/avd`       | **The CLI** (`avd`) — a thin client that talks to `avdd`.               |
| `Dockerfile`    | Builds a single image containing both binaries.                         |

Both binaries are written in **Go** and ship in one small container image.

---

## How it works

There are two independent planes, and keeping them separate is the whole design:

- **Control plane** — a small HTTP API (`avdd`). Start a device, stop it, list
  what's running. This is what the CLI talks to, and what you put a reverse proxy
  in front of.
- **Data plane** — raw **ADB over TCP** (and, optionally, a **noVNC** web view).
  These are direct connections to the device; they never pass through the HTTP
  API.

```
   client (CLI / agent)
      │  1. POST /devices                         ── control plane (HTTP) ──
      ▼
   avdd  ──▶ clones the golden snapshot
         ──▶ boots an emulator container (KVM-accelerated)
         ──▶ waits for sys.boot_completed
         ──▶ allocates a unique host port for ADB (and one for noVNC)
      │  2. returns { id, adb: "HOST:PORT", screen: "https://.../vnc/<id>" }
      ▼
   client
      3. adb connect HOST:PORT                     ── data plane (raw TCP) ──
      4. (optional) open the screen URL in a browser
      5. DELETE /devices/<id>   (or let the TTL reaper clean it up)
```

### Why each device gets its own port

Inside its container every emulator uses the standard ADB port (`5555`). `avdd`
publishes each container's `5555` on a **distinct host port** drawn from a
configured pool, and hands that port back in the start response. So two
concurrent devices look like:

| Device | Inside container | Published on host | Client connects with     |
| ------ | ---------------- | ----------------- | ------------------------ |
| A      | `:5555`          | `HOST:6001`       | `adb connect HOST:6001`  |
| B      | `:5555`          | `HOST:6002`       | `adb connect HOST:6002`  |

The client never guesses a port — it connects to exactly the address `avdd`
returned. `avdd` owns the bookkeeping: it tracks `session → port`, refuses to
double-allocate, and returns ports to the pool on teardown.

### Ephemeral by default

Each device is a **copy-on-start clone of a read-only golden snapshot** and is
destroyed on release. Device `/data` never persists between sessions. This makes
sessions cheap and clean, and it's why signing in to Google Play once (baked into
the golden snapshot) is enough for every future device to be Play-Store-ready.

### Pre-warmed snapshots

Cold-booting an emulator takes 30–90s. `avd-farm` boots from a **QuickBoot
snapshot** of an already-settled device, cutting session start to a few seconds.
The snapshot is built once and mounted read-only; sessions clone from it.

> Snapshots are tied to the GPU/render mode they were saved under — build the
> golden snapshot in the same render mode you run sessions with.

---

## Requirements

- **x86_64 host with KVM** (`/dev/kvm`) for hardware-accelerated emulation.
- **Docker** on the host. `avdd` orchestrates sibling containers, so it needs
  access to the Docker socket (see [Security](#security)).
- A **Play Store (google_apis_playstore) x86_64 system image** and a **golden
  snapshot** built from it.
- **Optional GPU:** an NVIDIA GPU + the NVIDIA Container Toolkit enables
  `-gpu host` rendering for smooth, graphics-heavy sessions. Without it, devices
  fall back to software rendering (`swiftshader_indirect`), which is perfectly
  fine for headless ADB / test workloads.

---

## Running the server

`avdd` runs as a container. It needs the Docker socket (to spawn emulator
containers), `/dev/kvm` (for acceleration), and — if you want GPU rendering —
the NVIDIA runtime.

```bash
docker run -d --name avdd \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --device /dev/kvm \
  -p 127.0.0.1:8700:8700 \
  -e AVD_BIND=0.0.0.0:8700 \
  -e AVD_ADB_PORT_RANGE=6000-6099 \
  -e AVD_VNC_PORT_RANGE=6100-6199 \
  -e AVD_MAX_DEVICES=3 \
  -e AVD_DEFAULT_TTL=2h \
  -e AVD_EMULATOR_IMAGE=ghcr.io/you/avd-emulator:latest \
  -e AVD_SNAPSHOT=golden \
  -e AVD_PUBLISH_HOST=<host-address-clients-will-use> \
  ghcr.io/you/avd-farm:latest
```

Note the API is bound to `127.0.0.1` on the host — `avd-farm` is meant to sit
**behind a reverse proxy**, not be exposed directly (see [Security](#security)).

### Configuration

| Env var               | Default        | Description                                              |
| --------------------- | -------------- | ------------------------------------------------------- |
| `AVD_BIND`            | `0.0.0.0:8700` | Address the HTTP API listens on inside the container.   |
| `AVD_ADB_PORT_RANGE`  | `6000-6099`    | Host port pool for ADB endpoints.                       |
| `AVD_VNC_PORT_RANGE`  | `6100-6199`    | Host port pool for noVNC endpoints.                     |
| `AVD_MAX_DEVICES`     | `3`            | Hard cap on concurrent devices (protects host RAM).     |
| `AVD_DEFAULT_TTL`     | `2h`           | Idle/lifetime TTL after which the reaper destroys a device. |
| `AVD_EMULATOR_IMAGE`  | —              | The emulator container image to launch.                 |
| `AVD_SNAPSHOT`        | `golden`       | Name of the QuickBoot snapshot to boot from.            |
| `AVD_GPU`             | `swiftshader`  | Render mode: `swiftshader` or `host`.                   |
| `AVD_PUBLISH_HOST`    | —              | Host/address embedded in the `adb`/`screen` responses.  |
| `AVD_DEVICE_MEMORY`   | `4g`           | Per-device container memory cap (`--memory`).           |
| `AVD_BOOT_TIMEOUT`    | `3m`           | How long to wait for `sys.boot_completed` before giving up (504). |

Each device is bound to a TTL at start; a background **reaper** destroys any
device whose TTL expires or that has been idle too long. This is the backstop
that guarantees a dropped client never strands a running device.

### Emulator image contract

The emulator image (`AVD_EMULATOR_IMAGE`) is an external dependency — this repo
does not build it. `avdd` assumes the image:

- boots the emulator from the QuickBoot snapshot named in the `SNAPSHOT` env var;
- exposes ADB on container port `5555` and noVNC on `6080`;
- honors the `GPU_MODE` (`swiftshader`|`host`), `SNAPSHOT`, and `API_LEVEL` env vars;
- ships `adb` inside the container, so `avdd` can poll
  `getprop sys.boot_completed` via `docker exec` to detect readiness.

---

## CLI

The `avd` CLI is a thin wrapper over the HTTP API. Point it at the control plane:

```bash
export AVD_ENDPOINT=https://avd.example.com
# credentials are for the upstream proxy that fronts the service
export AVD_USER=... AVD_PASS=...
```

```bash
# Start a device (blocks until booted), prints the endpoints
$ avd start --api 34 --gpu host --ttl 90m
id:     d3f9a1
adb:    <host>:6001
screen: https://avd.example.com/vnc/d3f9a1

# Convenience passthrough — runs adb against this session's device
$ avd adb d3f9a1 -- install ./app.apk
$ avd adb d3f9a1 -- logcat -d

# Print/open the screen URL for a visual session
$ avd screen d3f9a1

# List live devices, their ages and TTLs
$ avd list

# Tear it down now (otherwise the reaper will)
$ avd stop d3f9a1
```

---

## HTTP API

| Method   | Path             | Description                                             |
| -------- | ---------------- | ------------------------------------------------------ |
| `POST`   | `/devices`       | Start a device. Body: `{ api, gpu, ttl }`. Returns `{ id, adb, screen }`. |
| `GET`    | `/devices`       | List live devices.                                     |
| `GET`    | `/devices/{id}`  | Details for one device.                                |
| `DELETE` | `/devices/{id}`  | Stop and destroy a device.                             |
| `GET`    | `/healthz`       | Liveness/readiness probe.                              |

```bash
curl -s -X POST https://avd.example.com/devices \
  -H 'content-type: application/json' \
  -d '{"api":34,"gpu":"host","ttl":"90m"}'
# { "id":"d3f9a1", "adb":"<host>:6001", "screen":"https://avd.example.com/vnc/d3f9a1" }
```

---

## Security

**`avd-farm` performs no authentication of its own. It assumes every request it
receives has already been authenticated by an upstream reverse proxy** (for
example Traefik with basic-auth or forward-auth) and only forwards trusted
traffic. Deploy it accordingly:

- **Never expose the API directly.** Bind it to loopback (or an otherwise private
  interface) and put a proxy in front that authenticates and terminates TLS. The
  API itself has no login, no tokens, and no rate limiting — that is the proxy's
  job by design.
- **Keep it on a private network.** The control plane and, especially, the
  **ADB data plane are unauthenticated**. Anyone who can reach an ADB port has
  full control of that device (shell, install/uninstall, read app data). ADB
  cannot be fronted by an HTTP proxy, so its only protection is network reach —
  run `avd-farm` on a private/overlay network, not the public internet, and only
  publish ADB ports on a private interface.
- **The Docker socket is a privileged grant.** `avdd` mounts
  `/var/run/docker.sock` to spawn emulator containers, which is effectively
  root-equivalent control of the host's Docker. Treat the `avdd` container and
  image as sensitive, and rely on the fronting proxy to gate all access to it.
- **Treat installed apps as untrusted.** Sessions run arbitrary APKs. Devices are
  ephemeral with no `/data` persistence, and emulator containers are run isolated
  (no host bind mounts, resource-capped) so a misbehaving app can't pivot into
  the host.
- **Use a throwaway Google account** for the golden snapshot's Play sign-in. Those
  credentials live on every ephemeral device and are exposed to whatever the
  session installs — never bake in a personal account.

In short: **auth and TLS live in the proxy; network isolation guards ADB;
`avd-farm` trusts what reaches it.**

---

## Building

```bash
# Build both binaries into one image
docker build -t avd-farm:latest .

# Or build the binaries directly
go build -o bin/avdd ./cmd/avdd
go build -o bin/avd  ./cmd/avd
```

The `Dockerfile` is a multi-stage build: it compiles both `avdd` and `avd` from
source and copies the static binaries into a minimal runtime image.

---

## License

MIT
