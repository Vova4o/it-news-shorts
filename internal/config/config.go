// Package config loads and validates runtime configuration from environment
// variables without exposing secrets in logs or error messages.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAppEnv        = "development"
	defaultLogLevel      = "info"
	defaultTimezone      = "Europe/Moscow"
	defaultAPIListenAddr = ":8081"
	defaultWorkerMetrics = ":9090"

	defaultPGHost         = "127.0.0.1"
	defaultPGPort         = "5433"
	defaultPGUser         = "it_news_app"
	defaultPGDB           = "it_news"
	defaultPGSSLMode      = "disable"
	defaultPGMaxConns     = int32(10)
	defaultPGMinConns     = int32(1)
	defaultPGQueryTimeout = 5 * time.Second

	defaultMinIOEndpoint = "127.0.0.1:9000"
	defaultMinIOBucket   = "it-news-media"
	defaultMinIORegion   = "us-east-1"
	defaultMinIOMaxBytes = int64(256 << 20) // 256 MiB
	defaultMinIOPresign  = 15 * time.Minute
	defaultMinIOTimeout  = 30 * time.Second

	defaultTemporalAddress         = "127.0.0.1:7233"
	defaultTemporalNamespace       = "it-news"
	defaultTemporalTaskQueue       = "IT_NEWS_QUEUE"
	defaultTemporalActivityTimeout = 2 * time.Minute
	defaultTemporalWorkflowTimeout = 10 * time.Minute

	defaultNewsIngestionInterval = 30 * time.Minute
	defaultNewsHTTPTimeout       = 15 * time.Second
	defaultNewsHTTPMaxBytes      = int64(5 << 20) // 5 MiB
	defaultNewsMaxConcurrency    = 5
	defaultNewsUserAgent         = "it-news-shorts/1.0 (+https://github.com/it-news-shorts/it-news-shorts)"
	defaultNewsHNBaseURL         = "https://hacker-news.firebaseio.com/v0"
	defaultNewsHNLimit           = 30
	defaultNewsRSSLimit          = 50
)

// Config holds all infrastructure and runtime configuration.
type Config struct {
	App      AppConfig
	API      APIConfig
	Worker   WorkerConfig
	Postgres PostgresConfig
	MinIO    MinIOConfig
	Temporal TemporalConfig
	News     NewsConfig
}

// NewsConfig holds news collector, HTTP client, and schedule settings.
type NewsConfig struct {
	IngestionInterval time.Duration
	SchedulesEnabled  bool
	HTTPTimeout       time.Duration
	HTTPMaxBytes      int64
	MaxConcurrency    int
	UserAgent         string
	HNBaseURL         string
	HNLimit           int
	HNLists           []string
	RSSLimit          int
	ExtraRSSSources   string
}

// AppConfig holds general runtime settings.
type AppConfig struct {
	Env      string
	LogLevel string
	Timezone string
}

// APIConfig holds HTTP API server settings.
type APIConfig struct {
	ListenAddr      string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// WorkerConfig holds Temporal worker process settings.
type WorkerConfig struct {
	MetricsListenAddr string
	ShutdownTimeout   time.Duration
}

// PostgresConfig holds PostgreSQL connection pool and query settings.
type PostgresConfig struct {
	DSN          string
	MaxConns     int32
	MinConns     int32
	QueryTimeout time.Duration
}

// MinIOConfig holds S3-compatible object storage settings.
type MinIOConfig struct {
	Endpoint       string
	AccessKey      string
	SecretKey      string
	Bucket         string
	Region         string
	UseSSL         bool
	MaxObjectBytes int64
	PresignTTL     time.Duration
	RequestTimeout time.Duration
}

// TemporalConfig holds Temporal client and worker settings.
type TemporalConfig struct {
	Address         string
	Namespace       string
	TaskQueue       string
	ActivityTimeout time.Duration
	WorkflowTimeout time.Duration
}

// LoadFromEnv loads configuration using os.LookupEnv.
func LoadFromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

// Load reads and validates configuration using the supplied lookup function.
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}

	var cfg Config
	cfg.App = AppConfig{
		Env:      get("APP_ENV", defaultAppEnv),
		LogLevel: get("LOG_LEVEL", defaultLogLevel),
		Timezone: get("TZ", defaultTimezone),
	}

	var err error
	cfg.API.ListenAddr = get("API_LISTEN_ADDR", defaultAPIListenAddr)
	if cfg.API.ReadTimeout, err = parseDuration(lookup, "API_READ_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.API.WriteTimeout, err = parseDuration(lookup, "API_WRITE_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.API.ShutdownTimeout, err = parseDuration(lookup, "API_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}

	cfg.Worker.MetricsListenAddr = get("WORKER_METRICS_ADDR", defaultWorkerMetrics)
	if cfg.Worker.ShutdownTimeout, err = parseDuration(lookup, "WORKER_SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}

	cfg.Postgres.DSN = strings.TrimSpace(get("POSTGRES_DSN", ""))
	if cfg.Postgres.DSN == "" {
		host := get("POSTGRES_HOST", defaultPGHost)
		port := get("POSTGRES_PORT", defaultPGPort)
		user := get("POSTGRES_USER", defaultPGUser)
		pass := get("POSTGRES_PASSWORD", "")
		db := get("POSTGRES_DB", defaultPGDB)
		sslMode := get("POSTGRES_SSLMODE", defaultPGSSLMode)
		u := &url.URL{
			Scheme:   "postgres",
			Host:     net.JoinHostPort(host, port),
			Path:     "/" + db,
			RawQuery: "sslmode=" + url.QueryEscape(sslMode),
		}
		if pass != "" {
			u.User = url.UserPassword(user, pass)
		} else {
			u.User = url.User(user)
		}
		cfg.Postgres.DSN = u.String()
	}
	if cfg.Postgres.MaxConns, err = parseInt32(lookup, "POSTGRES_MAX_CONNS", defaultPGMaxConns); err != nil {
		return Config{}, err
	}
	if cfg.Postgres.MinConns, err = parseInt32(lookup, "POSTGRES_MIN_CONNS", defaultPGMinConns); err != nil {
		return Config{}, err
	}
	if cfg.Postgres.QueryTimeout, err = parseDuration(lookup, "POSTGRES_QUERY_TIMEOUT", defaultPGQueryTimeout); err != nil {
		return Config{}, err
	}

	cfg.MinIO.Endpoint = get("MINIO_ENDPOINT", defaultMinIOEndpoint)
	cfg.MinIO.AccessKey = get("MINIO_ACCESS_KEY", "")
	cfg.MinIO.SecretKey = get("MINIO_SECRET_KEY", "")
	cfg.MinIO.Bucket = get("MINIO_BUCKET", defaultMinIOBucket)
	cfg.MinIO.Region = get("MINIO_REGION", defaultMinIORegion)
	if cfg.MinIO.UseSSL, err = parseBool(lookup, "MINIO_USE_SSL", false); err != nil {
		return Config{}, err
	}
	if cfg.MinIO.MaxObjectBytes, err = parseInt64(lookup, "MINIO_MAX_OBJECT_BYTES", defaultMinIOMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.MinIO.PresignTTL, err = parseDuration(lookup, "MINIO_PRESIGN_TTL", defaultMinIOPresign); err != nil {
		return Config{}, err
	}
	if cfg.MinIO.RequestTimeout, err = parseDuration(lookup, "MINIO_REQUEST_TIMEOUT", defaultMinIOTimeout); err != nil {
		return Config{}, err
	}

	temporalAddr := get("TEMPORAL_ADDRESS", "")
	if temporalAddr == "" {
		temporalAddr = get("TEMPORAL_HOST", defaultTemporalAddress)
	}
	cfg.Temporal.Address = temporalAddr
	cfg.Temporal.Namespace = get("TEMPORAL_NAMESPACE", defaultTemporalNamespace)
	cfg.Temporal.TaskQueue = get("TEMPORAL_TASK_QUEUE", defaultTemporalTaskQueue)
	if cfg.Temporal.ActivityTimeout, err = parseDuration(lookup, "TEMPORAL_ACTIVITY_TIMEOUT", defaultTemporalActivityTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Temporal.WorkflowTimeout, err = parseDuration(lookup, "TEMPORAL_WORKFLOW_TIMEOUT", defaultTemporalWorkflowTimeout); err != nil {
		return Config{}, err
	}

	if cfg.News.IngestionInterval, err = parseDuration(lookup, "NEWS_INGESTION_INTERVAL", defaultNewsIngestionInterval); err != nil {
		return Config{}, err
	}
	if cfg.News.SchedulesEnabled, err = parseBool(lookup, "NEWS_SCHEDULES_ENABLED", true); err != nil {
		return Config{}, err
	}
	if cfg.News.HTTPTimeout, err = parseDuration(lookup, "NEWS_HTTP_TIMEOUT", defaultNewsHTTPTimeout); err != nil {
		return Config{}, err
	}
	if cfg.News.HTTPMaxBytes, err = parseInt64(lookup, "NEWS_HTTP_MAX_BYTES", defaultNewsHTTPMaxBytes); err != nil {
		return Config{}, err
	}
	maxConc, err := parseInt32(lookup, "NEWS_HTTP_MAX_CONCURRENCY", defaultNewsMaxConcurrency)
	if err != nil {
		return Config{}, err
	}
	cfg.News.MaxConcurrency = int(maxConc)
	cfg.News.UserAgent = get("NEWS_HTTP_USER_AGENT", defaultNewsUserAgent)
	cfg.News.HNBaseURL = strings.TrimRight(get("NEWS_HN_BASE_URL", defaultNewsHNBaseURL), "/")
	hnLimit, err := parseInt32(lookup, "NEWS_HN_LIMIT", defaultNewsHNLimit)
	if err != nil {
		return Config{}, err
	}
	cfg.News.HNLimit = int(hnLimit)
	rawLists := get("NEWS_HN_LISTS", "top,new,best")
	for _, part := range strings.Split(rawLists, ",") {
		p := strings.TrimSpace(strings.ToLower(part))
		if p != "" {
			cfg.News.HNLists = append(cfg.News.HNLists, p)
		}
	}
	rssLimit, err := parseInt32(lookup, "NEWS_RSS_LIMIT", defaultNewsRSSLimit)
	if err != nil {
		return Config{}, err
	}
	cfg.News.RSSLimit = int(rssLimit)
	cfg.News.ExtraRSSSources = get("NEWS_EXTRA_RSS_SOURCES", "")

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks structural validity of loaded configuration.
func (c Config) Validate() error {
	if c.Postgres.MaxConns < 1 {
		return fmt.Errorf("config: POSTGRES_MAX_CONNS must be >= 1")
	}
	if c.Postgres.MinConns < 0 || c.Postgres.MinConns > c.Postgres.MaxConns {
		return fmt.Errorf("config: POSTGRES_MIN_CONNS must be between 0 and POSTGRES_MAX_CONNS")
	}
	if c.Postgres.QueryTimeout <= 0 {
		return fmt.Errorf("config: POSTGRES_QUERY_TIMEOUT must be positive")
	}
	if strings.TrimSpace(c.MinIO.Bucket) == "" {
		return fmt.Errorf("config: MINIO_BUCKET must not be empty")
	}
	if c.MinIO.MaxObjectBytes <= 0 {
		return fmt.Errorf("config: MINIO_MAX_OBJECT_BYTES must be positive")
	}
	if c.MinIO.PresignTTL <= 0 {
		return fmt.Errorf("config: MINIO_PRESIGN_TTL must be positive")
	}
	if c.MinIO.RequestTimeout <= 0 {
		return fmt.Errorf("config: MINIO_REQUEST_TIMEOUT must be positive")
	}
	if strings.TrimSpace(c.Temporal.Address) == "" {
		return fmt.Errorf("config: TEMPORAL_ADDRESS must not be empty")
	}
	if strings.TrimSpace(c.Temporal.Namespace) == "" {
		return fmt.Errorf("config: TEMPORAL_NAMESPACE must not be empty")
	}
	if strings.TrimSpace(c.Temporal.TaskQueue) == "" {
		return fmt.Errorf("config: TEMPORAL_TASK_QUEUE must not be empty")
	}
	if c.Temporal.ActivityTimeout <= 0 {
		return fmt.Errorf("config: TEMPORAL_ACTIVITY_TIMEOUT must be positive")
	}
	if c.Temporal.WorkflowTimeout <= 0 {
		return fmt.Errorf("config: TEMPORAL_WORKFLOW_TIMEOUT must be positive")
	}
	if c.News.IngestionInterval <= 0 {
		return fmt.Errorf("config: NEWS_INGESTION_INTERVAL must be positive")
	}
	if c.News.HTTPTimeout <= 0 {
		return fmt.Errorf("config: NEWS_HTTP_TIMEOUT must be positive")
	}
	if c.News.HTTPMaxBytes <= 0 {
		return fmt.Errorf("config: NEWS_HTTP_MAX_BYTES must be positive")
	}
	if c.News.MaxConcurrency < 1 {
		return fmt.Errorf("config: NEWS_HTTP_MAX_CONCURRENCY must be >= 1")
	}
	if c.News.HNLimit < 1 {
		return fmt.Errorf("config: NEWS_HN_LIMIT must be >= 1")
	}
	if c.News.RSSLimit < 1 {
		return fmt.Errorf("config: NEWS_RSS_LIMIT must be >= 1")
	}
	return nil
}

// RedactDSN removes any password from a PostgreSQL DSN (URL, query string, or
// keyword/value format) for safe logging, failing closed on malformed input.
func RedactDSN(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") || strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "<invalid-dsn>"
		}
		if parsed.User != nil {
			if _, hasPass := parsed.User.Password(); hasPass {
				parsed.User = url.UserPassword(parsed.User.Username(), "REDACTED")
			}
		}
		if parsed.RawQuery != "" {
			q, err := url.ParseQuery(parsed.RawQuery)
			if err != nil {
				return "<invalid-dsn>"
			}
			changed := false
			for k := range q {
				if strings.EqualFold(k, "password") || strings.EqualFold(k, "pgpassword") {
					q.Set(k, "REDACTED")
					changed = true
				}
			}
			if changed {
				parsed.RawQuery = q.Encode()
			}
		}
		return parsed.String()
	}
	return redactKeywordDSN(trimmed)
}

func redactKeywordDSN(raw string) string {
	var tokens []string
	i := 0
	n := len(raw)
	for i < n {
		for i < n && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}
		keyStart := i
		for i < n && raw[i] != '=' && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '\n' && raw[i] != '\r' {
			i++
		}
		if i >= n || raw[i] != '=' || i == keyStart {
			return "<invalid-dsn>"
		}
		key := raw[keyStart:i]
		i++ // consume '='

		valStart := i
		if i < n && (raw[i] == '\'' || raw[i] == '"') {
			quote := raw[i]
			i++
			closed := false
			for i < n {
				if raw[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if quote == '\'' && raw[i] == '\'' && i+1 < n && raw[i+1] == '\'' {
					i += 2
					continue
				}
				if raw[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "<invalid-dsn>"
			}
		} else {
			for i < n && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '\n' && raw[i] != '\r' {
				if raw[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				i++
			}
		}
		rawVal := raw[valStart:i]
		if strings.EqualFold(key, "password") || strings.EqualFold(key, "pgpassword") {
			tokens = append(tokens, key+"=REDACTED")
		} else {
			tokens = append(tokens, key+"="+rawVal)
		}
	}
	if len(tokens) == 0 {
		return "<invalid-dsn>"
	}
	return strings.Join(tokens, " ")
}

func parseDuration(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("config: parse %s: %w", key, err)
	}
	return d, nil
}

func parseInt32(lookup func(string) (string, bool), key string, fallback int32) (int32, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("config: parse %s: %w", key, err)
	}
	return int32(n), nil
}

func parseInt64(lookup func(string) (string, bool), key string, fallback int64) (int64, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: parse %s: %w", key, err)
	}
	return n, nil
}

func parseBool(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("config: parse %s: %w", key, err)
	}
	return b, nil
}
