# Azure PostgreSQL Flexible Server + Managed Identity samples

| Folder | Language | Build tool | Key libraries |
|---|---|---|---|
| `java-sample/` | Java 17+ | Maven | `azure-identity-extensions` (JDBC auth plugin), pgjdbc, HikariCP |
| `go-sample/` | Go | go mod | `azidentity`, `pgx/v5` + `pgxpool` |

## How the token refresh works

Entra ID access tokens expire (typically after 1 to 24 hours). PostgreSQL only checks the token **when a connection logs in**. Both samples use the same approach:

1. **Get a token for each new connection.** The pool asks the Azure credential for a token with scope `https://ossrdbms-aad.database.windows.net/.default` every time it opens a physical connection, and uses it as the password.
   - Java: pgjdbc calls `AzurePostgresqlAuthenticationPlugin` (`authenticationPluginClassName`).
   - Go: the `pgxpool.Config.BeforeConnect` hook calls `cred.GetToken(...)`.
2. **Cache and refresh the token.** The Azure Identity SDK caches the token and gets a new one before it expires, so no refresh code is needed.
3. **Recycle connections.** `maxLifetime` (HikariCP) or `MaxConnLifetime` (pgx) is set to 30 minutes, so connections keep logging in again with a fresh token.

## Azure setup (one time)

```bash
# 1. Enable Entra auth on the server (can be combined with password auth)
az postgres flexible-server update -g <rg> -n <server> --microsoft-entra-auth Enabled

# 2. Set yourself as Entra admin, then connect as that admin and create a role for the managed identity:
#    SELECT * FROM pgaadauth_create_principal('<mi-name>', false, false);
#    GRANT ... ON ... TO "<mi-name>";
```

For a system-assigned identity, `<mi-name>` is the name of the VM / App Service / Container App. For a user-assigned identity, it is the identity resource's name.

## Environment variables (both samples)

| Variable | Required | Description |
|---|---|---|
| `PGHOST` | yes | `<server>.postgres.database.azure.com` |
| `PGUSER` | yes | The managed identity's PostgreSQL role name (`<mi-name>`) |
| `PGDATABASE` | no | Defaults to `postgres` |
| `AZURE_CLIENT_ID` | no | Client ID of a **user-assigned** managed identity. Leave unset for system-assigned. |
| `AZURE_USE_DEFAULT_CREDENTIAL` | no | `true` = use `DefaultAzureCredential` (for example `az login`) to test locally. Set `PGUSER` to your own Entra user. |

## Run

```bash
# Java
cd java-sample
mvn package
java -jar target/pg-managed-identity-sample-1.0.0.jar

# Go
cd go-sample
go run .
```

Each sample runs `SELECT current_user, now(), version()` every 30 seconds until stopped, so you can leave it running for hours and watch it keep working after tokens expire.
