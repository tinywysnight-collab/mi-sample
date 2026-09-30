# Java sample: Azure PostgreSQL Flexible Server with Managed Identity

Connects to Azure Database for PostgreSQL Flexible Server using a Managed Identity (no password). Tokens are refreshed automatically.

## Prerequisites

- JDK 17 or later
- Maven 3.9 or later
- Run it on an Azure resource that has a managed identity (VM, App Service, Container Apps, AKS with workload identity, and so on). To test locally, see [Run locally](#run-locally-without-a-managed-identity).
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
mvn package
```

This produces a runnable jar with all dependencies included: `target/pg-managed-identity-sample-1.0.0.jar`.

## Run

System-assigned managed identity:

```bash
export PGHOST=<server-name>.postgres.database.azure.com
export PGUSER=<mi-name>
export PGDATABASE=postgres
java -jar target/pg-managed-identity-sample-1.0.0.jar
```

User-assigned managed identity (also set the client ID):

```bash
export AZURE_CLIENT_ID=<client-id-of-the-user-assigned-identity>
java -jar target/pg-managed-identity-sample-1.0.0.jar
```

You can also run it without building the jar:

```bash
mvn compile exec:java
```

## Run locally (without a managed identity)

Sign in with the Azure CLI and use your own Entra account. Your account must be the server's Entra admin, or have its own role created with `pgaadauth_create_principal`.

```bash
az login
export AZURE_USE_DEFAULT_CREDENTIAL=true
export PGHOST=<server-name>.postgres.database.azure.com
export PGUSER=<your-entra-user@your-tenant.com>
java -jar target/pg-managed-identity-sample-1.0.0.jar
```

## Expected output

The app runs `SELECT current_user, now(), version()` every 30 seconds until you press `Ctrl+C`:

```
[2026-09-30T02:00:00Z] user=<mi-name> serverTime=2026-09-30 02:00:00.123
  PostgreSQL 16.x ...
```

## How the token expiration problem is solved

### The problem

With Managed Identity there is no fixed password. The app signs in with a Microsoft Entra **access token**, and that token expires, usually after 1 to 24 hours. A common mistake is to fetch a token once at startup and put it in the connection string as a fixed password. That works at first, but once the token expires, every **new** connection fails with `password authentication failed`, for example when the pool grows, reconnects after a network drop, or replaces a broken connection.

### The solution: three parts

**1. Get a token for every new connection, not once at startup**

The JDBC URL has no password. Instead, it registers an authentication plugin ([App.java](src/main/java/com/example/pgmi/App.java), `createDataSource`):

```java
config.addDataSourceProperty("authenticationPluginClassName",
        "com.azure.identity.extensions.jdbc.postgresql.AzurePostgresqlAuthenticationPlugin");
config.addDataSourceProperty("azure.managedIdentityEnabled", "true");
```

Every time pgjdbc opens a new physical connection, it calls this plugin. The plugin asks the credential for a token with scope `https://ossrdbms-aad.database.windows.net/.default` and sends it to the server as the password. Every login therefore uses a current token, never a stale one.

**2. The Azure Identity SDK caches the token and refreshes it early**

The credential keeps the token in memory and returns it from the cache while it is still valid. Shortly before it expires, the SDK gets a new token from the managed identity endpoint. So calling it on every connection is cheap, and the app needs no refresh code or timers of its own.

**3. The pool recycles connections regularly**

PostgreSQL checks the token **only when a connection logs in**. A connection that is already open keeps working after its token expires. To keep every connection's login recent, HikariCP retires connections on a schedule:

```java
config.setMaxLifetime(Duration.ofMinutes(30).toMillis());   // retire each connection after 30 minutes
config.setIdleTimeout(Duration.ofMinutes(10).toMillis());   // close idle connections
config.setKeepaliveTime(Duration.ofMinutes(5).toMillis());  // detect dead connections early
```

When a connection reaches `maxLifetime`, HikariCP closes it after it is returned to the pool and opens a new one. That new login goes through step 1 and gets a valid token. HikariCP never closes a connection while your code is using it.

### Timeline example

| Time | What happens |
|---|---|
| 00:00 | Pool starts. The plugin gets token A (valid for about 24h) and opens connection 1. |
| 00:30 | Connection 1 reaches `maxLifetime` and is replaced. The plugin returns token A from the cache. |
| ... | Connections keep being replaced every 30 minutes, each using the cached token. |
| ~23:xx | Token A is close to expiring. The SDK gets token B. |
| next replacement | New connections log in with token B. No errors, no restart. |

### Tuning

- Keep `maxLifetime` well below the token lifetime. 30 minutes is a safe default.
- Also keep `maxLifetime` a few minutes below any network or load-balancer idle timeout.
- For a user-assigned identity, set `AZURE_CLIENT_ID`. The plugin then asks for that identity's tokens (`azure.clientId`).

## Troubleshooting

- **`ManagedIdentityCredential authentication unavailable`**: the app is not running on an Azure resource with a managed identity. Enable one, or use `AZURE_USE_DEFAULT_CREDENTIAL=true` locally.
- **`password authentication failed for user ...`**: `PGUSER` does not match the role created with `pgaadauth_create_principal`, or the token belongs to a different identity. For a user-assigned identity, check `AZURE_CLIENT_ID`.
- **Connection timeout**: check the server's firewall or VNet rules.
