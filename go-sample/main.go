// Command go-sample connects to Azure Database for PostgreSQL Flexible Server
// with a Managed Identity.
//
// How token refresh works:
//   - pgxpool calls BeforeConnect every time it opens a new physical connection.
//     The hook asks the Azure credential for an access token (scope
//     https://ossrdbms-aad.database.windows.net/.default) and uses it as the password.
//   - azidentity caches the token and fetches a new one before it expires, so
//     every new connection authenticates with a valid token.
//   - PostgreSQL only validates the token at login time. MaxConnLifetime retires
//     connections periodically so the pool keeps re-authenticating with fresh tokens.
//
// Environment variables:
//
//	PGHOST                        e.g. myserver.postgres.database.azure.com
//	PGDATABASE                    database name (default: postgres)
//	PGUSER                        Entra principal name registered in PostgreSQL (the managed identity's name)
//	AZURE_CLIENT_ID               client ID of a user-assigned managed identity (omit for system-assigned)
//	AZURE_USE_DEFAULT_CREDENTIAL  "true" to use DefaultAzureCredential (e.g. az login) for local testing
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresScope = "https://ossrdbms-aad.database.windows.net/.default"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cred, err := newCredential()
	if err != nil {
		log.Fatalf("create credential: %v", err)
	}

	pool, err := newPool(ctx, cred)
	if err != nil {
		log.Fatalf("create pool: %v", err)
	}
	defer pool.Close()

	// Long-running loop so you can observe connections being recycled with new tokens.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		query(ctx, pool)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func newCredential() (azcore.TokenCredential, error) {
	if strings.EqualFold(os.Getenv("AZURE_USE_DEFAULT_CREDENTIAL"), "true") {
		return azidentity.NewDefaultAzureCredential(nil)
	}
	opts := &azidentity.ManagedIdentityCredentialOptions{}
	if clientID := os.Getenv("AZURE_CLIENT_ID"); clientID != "" {
		opts.ID = azidentity.ClientID(clientID)
	}
	return azidentity.NewManagedIdentityCredential(opts)
}

func newPool(ctx context.Context, cred azcore.TokenCredential) (*pgxpool.Pool, error) {
	host := mustEnv("PGHOST")
	user := mustEnv("PGUSER")
	database := envOrDefault("PGDATABASE", "postgres")

	dsn := fmt.Sprintf("postgres://%s@%s:5432/%s?sslmode=require",
		url.PathEscape(user), host, url.PathEscape(database))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	// Inject a (cached / auto-refreshed) Entra token as the password for every new connection.
	cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{postgresScope}})
		if err != nil {
			return fmt.Errorf("get access token: %w", err)
		}
		cc.Password = tok.Token
		return nil
	}

	cfg.MaxConns = 5
	cfg.MinConns = 1
	// Keep well below the token lifetime so connections are re-established with fresh tokens.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 2 * time.Minute
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func query(ctx context.Context, pool *pgxpool.Pool) {
	var user, version string
	var now time.Time
	err := pool.QueryRow(ctx, "SELECT current_user, now(), version()").Scan(&user, &now, &version)
	if err != nil {
		log.Printf("query failed: %v", err)
		return
	}
	log.Printf("user=%s serverTime=%s\n  %s", user, now.Format(time.RFC3339), version)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("environment variable %s is required", name)
	}
	return v
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
