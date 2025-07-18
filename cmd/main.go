package main

import (
	"os"
	"time"
	_ "time/tzdata"

	"github.com/frain-dev/convoy/cmd/ff"
	"github.com/frain-dev/convoy/cmd/utils"

	"github.com/frain-dev/convoy/cmd/agent"
	"github.com/frain-dev/convoy/cmd/bootstrap"

	configCmd "github.com/frain-dev/convoy/cmd/config"
	"github.com/frain-dev/convoy/cmd/hooks"
	"github.com/frain-dev/convoy/cmd/migrate"
	"github.com/frain-dev/convoy/cmd/retry"
	"github.com/frain-dev/convoy/cmd/server"
	"github.com/frain-dev/convoy/cmd/stream"
	"github.com/frain-dev/convoy/cmd/version"
	"github.com/frain-dev/convoy/database/postgres"
	"github.com/sirupsen/logrus"

	"github.com/frain-dev/convoy/internal/pkg/cli"

	"github.com/frain-dev/convoy"
	"github.com/frain-dev/convoy/cmd/openapi"
)

func main() {
	slog := logrus.New()
	slog.Out = os.Stdout

	err := os.Setenv("TZ", "") // Use UTC by default :)
	if err != nil {
		slog.Fatal("failed to set env - ", err)
	}

	app := &cli.App{}
	app.Version = convoy.GetVersionFromFS(convoy.F)
	db := &postgres.Postgres{}

	c := cli.NewCli(app)

	var dbPort int
	var dbType string
	var dbHost string
	var dbScheme string
	var dbUsername string
	var dbPassword string
	var dbDatabase string
	var dbReadReplicasDSN []string

	var fflag []string
	var ipAllowList []string
	var ipBLockList []string
	var enableProfiling bool

	var redisPort int
	var redisHost string
	var redisType string
	var redisScheme string
	var redisUsername string
	var redisPassword string
	var redisDatabase string

	var tracerType string
	var sentryDSN string
	var sentrySampleRate float64
	var otelSampleRate float64
	var otelCollectorURL string
	var otelAuthHeaderName string
	var otelAuthHeaderValue string
	var dataDogAgentUrl string
	var metricsBackend string
	var prometheusMetricsSampleTime uint64

	var retentionPolicy string
	var retentionPolicyEnabled bool

	var maxRetrySeconds uint64

	var instanceIngestRate int
	var apiRateLimit int

	var licenseKey string
	var logLevel string

	var configFile string

	c.Flags().StringVar(&configFile, "config", "./convoy.json", "Configuration file for convoy")
	c.Flags().StringVar(&licenseKey, "license-key", "", "Convoy license key")
	c.Flags().StringVar(&logLevel, "log-level", "", "Log level")

	// db config
	c.Flags().StringVar(&dbHost, "db-host", "", "Database Host")
	c.Flags().StringVar(&dbType, "db-type", "", "Database provider")
	c.Flags().StringVar(&dbScheme, "db-scheme", "", "Database Scheme")
	c.Flags().StringVar(&dbUsername, "db-username", "", "Database Username")
	c.Flags().StringVar(&dbPassword, "db-password", "", "Database Password")
	c.Flags().StringVar(&dbDatabase, "db-database", "", "Database Database")
	c.Flags().StringVar(&dbDatabase, "db-options", "", "Database Options")
	c.Flags().IntVar(&dbPort, "db-port", 0, "Database Port")
	c.Flags().BoolVar(&enableProfiling, "enable-profiling", false, "Enable profiling and exporting profile data to pyroscope")
	c.Flags().StringSliceVar(&dbReadReplicasDSN, "read-replicas-dsn", []string{}, "Comma-separated list of read replica DSNs e.g. postgres://convoy:convoy@host1:5436/db,postgres://convoy:convoy@host2:5437/db")

	// redis config
	c.Flags().StringVar(&redisHost, "redis-host", "", "Redis Host")
	c.Flags().StringVar(&redisType, "redis-type", "", "Redis provider")
	c.Flags().StringVar(&redisScheme, "redis-scheme", "", "Redis Scheme")
	c.Flags().StringVar(&redisUsername, "redis-username", "", "Redis Username")
	c.Flags().StringVar(&redisPassword, "redis-password", "", "Redis Password")
	c.Flags().StringVar(&redisDatabase, "redis-database", "", "Redis database")
	c.Flags().IntVar(&redisPort, "redis-port", 0, "Redis Port")

	// misc
	c.Flags().StringSliceVar(&fflag, "enable-feature-flag", []string{}, "List of feature flags to enable e.g. \"full-text-search,prometheus\"")
	c.Flags().StringSliceVar(&ipAllowList, "ip-allow-list", []string{}, "List of IPs CIDRs to allow e.g. \" 0.0.0.0/0,127.0.0.0/8\"")
	c.Flags().StringSliceVar(&ipBLockList, "ip-block-list", []string{}, "List of IPs CIDRs to block e.g. \" 0.0.0.0/0,127.0.0.0/8\"")

	c.Flags().IntVar(&instanceIngestRate, "instance-ingest-rate", 0, "Instance ingest Rate")
	c.Flags().IntVar(&apiRateLimit, "api-rate-limit", 0, "API rate limit")

	// tracing
	c.Flags().StringVar(&tracerType, "tracer-type", "", "Tracer backend, e.g. sentry, datadog or otel")
	c.Flags().StringVar(&sentryDSN, "sentry-dsn", "", "Sentry backend dsn")
	c.Flags().Float64Var(&sentrySampleRate, "sentry-sample-rate", 1.0, "Sentry tracing sample rate")
	c.Flags().Float64Var(&otelSampleRate, "otel-sample-rate", 1.0, "OTel tracing sample rate")
	c.Flags().StringVar(&otelCollectorURL, "otel-collector-url", "", "OTel collector URL")
	c.Flags().StringVar(&otelAuthHeaderName, "otel-auth-header-name", "", "OTel backend auth header name")
	c.Flags().StringVar(&otelAuthHeaderValue, "otel-auth-header-value", "", "OTel backend auth header value")
	c.Flags().StringVar(&dataDogAgentUrl, "datadog-agent-url", "", "Datadog agent URL")

	// metrics
	c.Flags().StringVar(&metricsBackend, "metrics-backend", "prometheus", "Metrics backend e.g. prometheus. ('prometheus' feature flag required")
	c.Flags().Uint64Var(&prometheusMetricsSampleTime, "metrics-prometheus-sample-time", 5, "Prometheus metrics sample time")

	c.Flags().StringVar(&retentionPolicy, "retention-policy", "", "Retention Policy Duration")
	c.Flags().BoolVar(&retentionPolicyEnabled, "retention-policy-enabled", false, "Retention Policy Enabled")

	c.Flags().Uint64Var(&maxRetrySeconds, "max-retry-seconds", 7200, "Max retry seconds exponential backoff")

	AddHCPVaultFlags(c)

	c.PersistentPreRunE(hooks.PreRun(app, db))
	c.PersistentPostRunE(hooks.PostRun(app, db))

	c.AddCommand(version.AddVersionCommand())
	c.AddCommand(server.AddServerCommand(app))
	c.AddCommand(retry.AddRetryCommand(app))
	c.AddCommand(migrate.AddMigrateCommand(app))
	c.AddCommand(configCmd.AddConfigCommand(app))
	c.AddCommand(stream.AddStreamCommand(app))
	c.AddCommand(bootstrap.AddBootstrapCommand(app))
	c.AddCommand(agent.AddAgentCommand(app))
	c.AddCommand(ff.AddFeatureFlagsCommand())
	c.AddCommand(utils.AddUtilsCommand(app))
	c.AddCommand(openapi.AddOpenAPICommand())

	if err = c.Execute(); err != nil {
		slog.Fatal(err)
	}
}

func AddHCPVaultFlags(c *cli.ConvoyCli) {
	c.Flags().String("hcp-client-id", "", "HCP Vault client ID")
	c.Flags().String("hcp-client-secret", "", "HCP Vault client secret")
	c.Flags().String("hcp-org-id", "", "HCP Vault organization ID")
	c.Flags().String("hcp-project-id", "", "HCP Vault project ID")
	c.Flags().String("hcp-app-name", "", "HCP Vault app name")
	c.Flags().String("hcp-secret-name", "", "HCP Vault secret name")

	// New flag for cache duration
	c.Flags().Duration("hcp-cache-duration", 5*time.Minute, "HCP Vault key cache duration")
}
