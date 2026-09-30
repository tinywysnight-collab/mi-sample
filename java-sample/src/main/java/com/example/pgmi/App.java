package com.example.pgmi;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;

import java.sql.Connection;
import java.sql.ResultSet;
import java.sql.Statement;
import java.time.Duration;
import java.time.Instant;

/**
 * Connects to Azure Database for PostgreSQL Flexible Server with a Managed Identity.
 *
 * <p>How token refresh works:
 * <ul>
 *   <li>The pgjdbc driver calls {@code AzurePostgresqlAuthenticationPlugin} every time it opens a
 *       new physical connection. The plugin asks the Azure credential for an access token
 *       (scope {@code https://ossrdbms-aad.database.windows.net/.default}) and uses it as the password.</li>
 *   <li>The credential caches the token and transparently fetches a new one before it expires,
 *       so every new connection always authenticates with a valid token.</li>
 *   <li>PostgreSQL only validates the token at login time. HikariCP's {@code maxLifetime} retires
 *       connections periodically so the pool keeps re-authenticating with fresh tokens.</li>
 * </ul>
 *
 * <p>Environment variables:
 * <ul>
 *   <li>{@code PGHOST}     – e.g. myserver.postgres.database.azure.com</li>
 *   <li>{@code PGDATABASE} – database name (default: postgres)</li>
 *   <li>{@code PGUSER}     – the Entra principal name registered in PostgreSQL (the managed identity's name)</li>
 *   <li>{@code AZURE_CLIENT_ID} – client ID of a user-assigned managed identity (omit for system-assigned)</li>
 *   <li>{@code AZURE_USE_DEFAULT_CREDENTIAL=true} – use DefaultAzureCredential (e.g. az login) for local testing</li>
 * </ul>
 */
public final class App {

    private static final String AUTH_PLUGIN =
            "com.azure.identity.extensions.jdbc.postgresql.AzurePostgresqlAuthenticationPlugin";

    public static void main(String[] args) throws Exception {
        String host = requireEnv("PGHOST");
        String database = envOrDefault("PGDATABASE", "postgres");
        String user = requireEnv("PGUSER");
        String clientId = System.getenv("AZURE_CLIENT_ID");
        boolean useDefaultCredential = Boolean.parseBoolean(System.getenv("AZURE_USE_DEFAULT_CREDENTIAL"));

        try (HikariDataSource dataSource = createDataSource(host, database, user, clientId, useDefaultCredential)) {
            // Long-running loop so you can observe connections being recycled with new tokens.
            while (true) {
                try (Connection conn = dataSource.getConnection();
                     Statement stmt = conn.createStatement();
                     ResultSet rs = stmt.executeQuery("SELECT current_user, now(), version()")) {
                    if (rs.next()) {
                        System.out.printf("[%s] user=%s serverTime=%s%n  %s%n",
                                Instant.now(), rs.getString(1), rs.getTimestamp(2), rs.getString(3));
                    }
                } catch (Exception e) {
                    System.err.printf("[%s] query failed: %s%n", Instant.now(), e.getMessage());
                }
                Thread.sleep(Duration.ofSeconds(30).toMillis());
            }
        }
    }

    static HikariDataSource createDataSource(String host, String database, String user,
                                             String clientId, boolean useDefaultCredential) {
        HikariConfig config = new HikariConfig();
        config.setJdbcUrl("jdbc:postgresql://" + host + ":5432/" + database + "?sslmode=require");
        config.setUsername(user);
        // No password: the authentication plugin supplies an Entra access token instead.
        config.addDataSourceProperty("authenticationPluginClassName", AUTH_PLUGIN);
        if (!useDefaultCredential) {
            config.addDataSourceProperty("azure.managedIdentityEnabled", "true");
        }
        if (clientId != null && !clientId.isBlank()) {
            config.addDataSourceProperty("azure.clientId", clientId);
        }

        config.setPoolName("pg-mi-pool");
        config.setMaximumPoolSize(5);
        config.setMinimumIdle(1);
        // Keep well below the token lifetime so connections are re-established with fresh tokens.
        config.setMaxLifetime(Duration.ofMinutes(30).toMillis());
        config.setIdleTimeout(Duration.ofMinutes(10).toMillis());
        config.setKeepaliveTime(Duration.ofMinutes(5).toMillis());
        config.setConnectionTimeout(Duration.ofSeconds(30).toMillis());
        return new HikariDataSource(config);
    }

    private static String requireEnv(String name) {
        String value = System.getenv(name);
        if (value == null || value.isBlank()) {
            throw new IllegalStateException("Environment variable " + name + " is required");
        }
        return value;
    }

    private static String envOrDefault(String name, String defaultValue) {
        String value = System.getenv(name);
        return (value == null || value.isBlank()) ? defaultValue : value;
    }

    private App() {
    }
}
