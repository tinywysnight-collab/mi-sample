# Go sample: Azure PostgreSQL Flexible Server with Managed Identity

Connects to Azure Database for PostgreSQL Flexible Server using a Managed Identity (no password). Tokens are refreshed automatically.

## Prerequisites

- Go 1.26 or later (see `go.mod`)
- Run it on an Azure resource that has a managed identity (VM, App Service, Container Apps, AKS, and so on). To test locally, see [Run locally](#run-locally-without-a-managed-identity).
- Microsoft Entra authentication is enabled on the PostgreSQL server:

  ```bash
  az postgres flexible-server update -g <resource-group> -n <server-name> --microsoft-entra-auth Enabled
  ```

- A PostgreSQL role exists for the managed identity. Connect as the Entra admin to the `postgres` database and run:

  ```sql
  SELECT * FROM pgaadauth_create_principal('<mi-name>', false, false);
  -- then grant what the app needs, for example:
  GRANT CONNECT ON DATABASE <db> TO "<mi-name>";
  ```

  `<mi-name>` is the resource name for a system-assigned identity (for example the VM or web app name), or the identity's name for a user-assigned identity.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `PGHOST` | yes | `<server-name>.postgres.database.azure.com` |
| `PGUSER` | yes | The managed identity's PostgreSQL role name (`<mi-name>`) |
| `PGDATABASE` | no | Database name. Defaults to `postgres`. |
| `AZURE_CLIENT_ID` | no | Client ID of a **user-assigned** managed identity. Leave unset for system-assigned. |
| `AZURE_USE_DEFAULT_CREDENTIAL` | no | Set to `true` to use `DefaultAzureCredential` instead of the managed identity (for local testing). |

## Build

```bash
go mod download
go build -o pg-mi-sample .
```

To build a Linux binary on another OS (for example to copy it to an Azure VM or a container):

```bash
GOOS=linux GOARCH=amd64 go build -o pg-mi-sample .
```

## Run

System-assigned managed identity:

```bash
export PGHOST=<server-name>.postgres.database.azure.com
export PGUSER=<mi-name>
export PGDATABASE=postgres
./pg-mi-sample
```

User-assigned managed identity (also set the client ID):

```bash
export AZURE_CLIENT_ID=<client-id-of-the-user-assigned-identity>
./pg-mi-sample
```

You can also run it without building a binary:

```bash
go run .
```

## Run locally (without a managed identity)

Sign in with the Azure CLI and use your own Entra account. Your account must be the server's Entra admin, or have its own role created with `pgaadauth_create_principal`.

```bash
az login
export AZURE_USE_DEFAULT_CREDENTIAL=true
export PGHOST=<server-name>.postgres.database.azure.com
export PGUSER=<your-entra-user@your-tenant.com>
go run .
```

## Expected output

The app runs `SELECT current_user, now(), version()` every 30 seconds until you press `Ctrl+C`:

```
2026/09/30 10:00:00 user=<mi-name> serverTime=2026-09-30T02:00:00Z
  PostgreSQL 16.x ...
```

## How the token expiration problem is solved

### The problem

With Managed Identity there is no fixed password. The app signs in with a Microsoft Entra **access token**, and that token expires, usually after 1 to 24 hours. A common mistake is to fetch a token once at startup and put it in the DSN as a fixed password. That works at first, but once the token expires, every **new** connection fails with `password authentication failed`, for example when the pool grows, reconnects after a network drop, or replaces a broken connection.

### The solution: three parts

**1. Get a token for every new connection, not once at startup**

The DSN has no password. Instead, a `BeforeConnect` hook adds the token each time ([main.go](main.go), `newPool`):

```go
cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
    tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{
        Scopes: []string{"https://ossrdbms-aad.database.windows.net/.default"},
    })
    if err != nil {
        return fmt.Errorf("get access token: %w", err)
    }
    cc.Password = tok.Token
    return nil
}
```

`pgxpool` runs this hook every time it opens a new physical connection, so every login uses a current token, never a stale one.

**2. azidentity caches the token and refreshes it early**

`ManagedIdentityCredential` (and `DefaultAzureCredential`) keep the token in memory and return it from the cache while it is still valid. Shortly before it expires, azidentity gets a new token from the managed identity endpoint. So calling `GetToken` on every connection is cheap, and the app needs no refresh code or goroutines of its own.

**3. The pool recycles connections regularly**

PostgreSQL checks the token **only when a connection logs in**. A connection that is already open keeps working after its token expires. To keep every connection's login recent, `pgxpool` retires connections on a schedule:

```go
cfg.MaxConnLifetime = 30 * time.Minute        // retire each connection after 30 minutes
cfg.MaxConnLifetimeJitter = 2 * time.Minute   // spread out replacements so they don't all happen at once
cfg.MaxConnIdleTime = 10 * time.Minute        // close idle connections
cfg.HealthCheckPeriod = time.Minute           // how often the pool checks lifetimes and health
```

When a connection reaches `MaxConnLifetime`, the pool closes it once it is idle and opens a new one when needed. That new login goes through step 1 and gets a valid token. The pool never closes a connection while your code is using it.

### Timeline example

| Time | What happens |
|---|---|
| 00:00 | Pool starts. `BeforeConnect` gets token A (valid for about 24h) and opens connection 1. |
| ~00:30 | Connection 1 reaches `MaxConnLifetime` and is replaced. `GetToken` returns token A from the cache. |
| ... | Connections keep being replaced about every 30 minutes, each using the cached token. |
| ~23:xx | Token A is close to expiring. azidentity gets token B. |
| next replacement | New connections log in with token B. No errors, no restart. |

### Tuning

- Keep `MaxConnLifetime` well below the token lifetime. 30 minutes is a safe default.
- Also keep `MaxConnLifetime` a few minutes below any network or load-balancer idle timeout.
- Create the credential **once** and reuse it, as `main` does. A new credential per connection would lose the token cache.
- For a user-assigned identity, set `AZURE_CLIENT_ID` so the credential asks for that identity's tokens.

## Troubleshooting

- **`create pool: ping: context deadline exceeded`** at startup: the app could not get a token or reach the server within 30 seconds. Off Azure, this usually means there is no managed identity endpoint. Use `AZURE_USE_DEFAULT_CREDENTIAL=true` locally.
- **`password authentication failed for user ...`**: `PGUSER` does not match the role created with `pgaadauth_create_principal`, or the token belongs to a different identity. For a user-assigned identity, check `AZURE_CLIENT_ID`.
- **Connection timeout**: check the server's firewall or VNet rules.
