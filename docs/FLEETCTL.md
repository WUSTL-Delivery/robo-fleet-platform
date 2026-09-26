# fleetctl

`fleetctl` is the admin CLI for a running `fleet-server` (DESIGN.md D14). It creates
operator invites, manages enrollment keys, and lists or revokes clients. It talks only to
the server's admin HTTP API; it never opens the database. It is a separate binary from
`fleet-server` and uses only the Go standard library.

```bash
cd server && go build -o bin/fleetctl ./cmd/fleetctl     # until the curl installer ships
```

## Before you start

The server must have `FLEET_ADMIN_TOKEN` set (at least 16 characters). Without it the
admin API is not mounted, and every command fails with
`admin API not found: is FLEET_ADMIN_TOKEN set on the server?`.

Log in once:

```bash
fleetctl login --server https://fleet.bearcarts.com          # prompts for the admin token on stdin
fleetctl login --server http://127.0.0.1:8080 --token "$T"   # or pass it (ends up in shell history)
```

`login` checks `<server>/healthz` answers, then saves `{server, token}` to
`$XDG_CONFIG_HOME/fleetctl/config.json` (default `~/.config/fleetctl/config.json`, mode
0600). It does **not** check the token; a wrong token shows up on the first real command.

**For scripts and agents, skip `login`** and set both variables. They override the saved
config, and nothing is written to disk:

```bash
export FLEETCTL_SERVER=https://fleet.bearcarts.com
export FLEETCTL_TOKEN=<admin token>
```

## Commands

`<name>` is the fleet **name** (e.g. `club-fleet`, `demo`), not its `f_...` id. Durations
are Go duration strings: `90m`, `24h`, `720h`.

| Command | What it does | Repeat-safe | Side effects |
|---|---|---|---|
| `invite operator --fleet <name> [--ttl 24h]` | Creates a single-use operator invite (`fp-oi-...`). Default ttl 24h, max 720h | no, each run creates a new invite | none until redeemed |
| `enroll-key create --fleet <name> [--ttl 720h]` | Creates an enrollment key (`fp-ek-...`) that robots and services enroll with. No `--ttl` means it never expires | no, each run creates a new key | none until used |
| `enroll-key list --fleet <name>` | Lists keys: `ID STATE CREATED EXPIRES REVOKED`. `STATE` is `active`, `expired` or `revoked`. Never prints the key itself | yes (read only) | none |
| `enroll-key revoke --fleet <name> <ek_id>` | Stops new enrollments with that key | yes | clients that already enrolled with it keep their tokens |
| `client list --fleet <name>` | Lists robots, services and operators: `ID KIND NAME STATE CREATED REVOKED`. Never prints tokens | yes (read only) | none |
| `client revoke --fleet <name> <client_id>` | Revokes one client's token and closes its live connection | yes (a second revoke closes nothing) | **immediate**: see below |
| `version` / `--version` | Prints the fleetctl version | yes | none |

The key or invite printed by `invite operator` and `enroll-key create` is shown **once**.
The server stores only its hash, so it cannot be listed or recovered later. Keep it from
that one output.

Client ids start with `r_` (robot), `s_` (service) or `o_` (operator). Enrollment key ids
start with `ek_`. Both come from the `list` commands.

### What `client revoke` does

- If the client is connected, its socket gets `error{code: auth_failed, "token revoked"}`
  and is closed right away.
- A **robot** goes `robot.offline` for every subscriber. If an operator was driving it,
  that lease ends too.
- An **operator** loses every lease it held (`operator_lost`), and those robots go to
  `HELP_REQUESTED`.
- Every later `hello` with that token is refused with `auth_failed`. Both SDKs stop on
  it and do not reconnect.
- It cannot be undone. To bring the machine back, enroll it again as a new client with an
  enrollment key; it gets a new id.

### Enrollment keys and bootstrap

`FLEET_BOOTSTRAP_ENROLL_KEY` registers its key on every server start. Once that key is
revoked with `enroll-key revoke`, it **stays revoked** after a restart; the server logs a
warning and starts anyway. To rotate: `enroll-key create`, give robots the new key,
`enroll-key revoke` the old one, then change the environment variable.

## Output and exit codes

Output is human-formatted text on stdout, with tables for `list`. There is no `--json`
flag yet. For machine-readable data, call the admin API directly (next section).

| Exit | Meaning |
|---|---|
| `0` | success, or `--help` |
| `1` | request failed: not logged in, server unreachable, admin token rejected, unknown fleet or id, bad ttl, or an unexpected server response |
| `2` | usage error: unknown command or subcommand, missing `--fleet`, unexpected argument |

Errors go to stderr as `fleetctl: <message>`. The messages to match on:

| Message contains | Cause |
|---|---|
| `not logged in` | no saved login and `FLEETCTL_SERVER` / `FLEETCTL_TOKEN` not set |
| `cannot reach` | server down, wrong URL, or network/DNS blocked |
| `admin token rejected` | wrong `FLEETCTL_TOKEN` (HTTP 401) |
| `admin API not found` | server has no `FLEET_ADMIN_TOKEN` |
| `fleet "<name>": ...` | no fleet with that name, or the id is not in that fleet (HTTP 404) |

## Admin API (for agents that want JSON)

Every route takes `Authorization: Bearer <admin token>`. `{fleet}` is the fleet name.
Errors come back as `{"error": "..."}` with 400, 401 or 404.

| Method and path | Body | 200 response |
|---|---|---|
| `POST /api/admin/fleets/{fleet}/operator-invites` | optional `{"ttl":"24h"}` | `{key, fleet_id, expires_at}` |
| `POST /api/admin/fleets/{fleet}/enroll-keys` | optional `{"ttl":"720h"}` | `{key, id, fleet_id, created_at, expires_at?, state}` |
| `GET  /api/admin/fleets/{fleet}/enroll-keys` | | `{enroll_keys: [{id, fleet_id, created_at, expires_at?, revoked_at?, state}]}` |
| `POST /api/admin/fleets/{fleet}/enroll-keys/{id}/revoke` | | the key's record |
| `GET  /api/admin/fleets/{fleet}/clients` | | `{clients: [{id, fleet_id, kind, name, created_at, revoked_at?, state}]}` |
| `POST /api/admin/fleets/{fleet}/clients/{id}/revoke` | | the client's record plus `disconnected` (bool) |

```bash
curl -s -H "Authorization: Bearer $FLEETCTL_TOKEN" \
  "$FLEETCTL_SERVER/api/admin/fleets/club-fleet/clients" | jq '.clients[] | select(.kind=="robot")'
```

## Recipes

**Onboard an operator.** Run `fleetctl invite operator --fleet club-fleet`. Send the
printed key to the person; they paste it on the console login screen at the server's `/`
within the ttl. It works once.

**Add a robot.** Robots enroll themselves with an enrollment key (the fleet's bootstrap
key, or one from `enroll-key create`). Give the key to the robot as `FLEET_ENROLL_KEY`;
after its first connect it keeps its own token and no longer needs the key.

**Retire a lost robot or laptop.** Run `fleetctl client list --fleet club-fleet` to find
its `r_...` / `o_...` id, then `fleetctl client revoke --fleet club-fleet <id>`.

**A key leaked.** `enroll-key list` to find its `ek_...` id, then `enroll-key revoke`.
Robots already enrolled are unaffected. To also cut off a robot that enrolled with the
leaked key, revoke that client.

## Rules for agents

- **Ask a human before any `revoke`.** `client revoke` drops a live robot or operator
  immediately and cannot be undone. `enroll-key revoke` can lock new robots out of the
  fleet.
- Read-only commands (`list`, `version`) are always safe to run.
- `invite operator` and `enroll-key create` print a secret. Don't echo it into logs,
  commits, tickets or chat beyond handing it to the person who asked for it.
- Never put the admin token in a command line that gets logged; use `FLEETCTL_TOKEN`.
- Never edit the server's sqlite database to do any of this. Everything goes through the
  admin API.
