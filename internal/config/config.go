package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

// encryptionKeySize is the key length AES-256-GCM requires.
const encryptionKeySize = 32

// SecretExpiryWarning is how early Warnings starts reporting a provider
// client secret that is about to expire.
const SecretExpiryWarning = 30 * 24 * time.Hour

// OAuthClient is one provider's OAuth app registration.
type OAuthClient struct {
	ClientID     string
	ClientSecret string // never log it
	RedirectURL  string
}

// MicrosoftClient is the Entra ID app registration.
type MicrosoftClient struct {
	OAuthClient
	// Tenant is MICROSOFT_TENANT_ID: "common" (work/school + personal
	// accounts), "organizations", "consumers", a directory GUID or a domain.
	Tenant string
	// SecretExpiresAt is MICROSOFT_EXPIRATION_DATE (00:00 UTC that day);
	// zero when unset. Entra ID client secrets always expire.
	SecretExpiresAt time.Time
}

type Config struct {
	Port        string
	DatabaseURL string
	LogLevel    string

	// Env is RELAY_ENV: which API key prefix (rly_live_ / rly_test_) this
	// process accepts. Required, with no default, so production can never
	// start in test mode by accident (or the other way around).
	Env apikey.Env

	// Providers are optional: nil means not configured. At least one is required.
	Google    *OAuthClient
	Microsoft *MicrosoftClient

	// EncryptionKey is the decoded ENCRYPTION_KEY (32 bytes). Never log it.
	EncryptionKey []byte

	// RefreshAhead is how early the background refresher renews an access
	// token (REFRESH_AHEAD, default 10m). RefreshConcurrency is how many
	// provider refresh calls run at once (REFRESH_CONCURRENCY, default 8).
	RefreshAhead       time.Duration
	RefreshConcurrency int
}

// Load reads the environment and fails fast: every missing or invalid
// variable is reported together so the process never starts half-configured.
func Load() (Config, error) {
	return load(time.Now())
}

func load(now time.Time) (Config, error) {
	cfg := Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		LogLevel:    getEnv("LOG_LEVEL", "info"),
	}

	var errs []error
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	env, err := ParseEnv(os.Getenv("RELAY_ENV"))
	if err != nil {
		errs = append(errs, fmt.Errorf("RELAY_ENV: %w", err))
	}
	cfg.Env = env

	google, gErrs := loadOAuthClient("GOOGLE")
	errs = append(errs, gErrs...)
	cfg.Google = google

	microsoft, mErrs := loadMicrosoft(now)
	errs = append(errs, mErrs...)
	cfg.Microsoft = microsoft

	if google == nil && microsoft == nil && len(gErrs) == 0 && len(mErrs) == 0 {
		errs = append(errs, errors.New("no provider configured: set GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET and/or MICROSOFT_CLIENT_ID/MICROSOFT_CLIENT_SECRET"))
	}

	key, err := decodeEncryptionKey(os.Getenv("ENCRYPTION_KEY"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ENCRYPTION_KEY: %w", err))
	}
	cfg.EncryptionKey = key

	ahead, err := durationEnv("REFRESH_AHEAD", 10*time.Minute)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.RefreshAhead = ahead
	n, err := positiveIntEnv("REFRESH_CONCURRENCY", 8)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.RefreshConcurrency = n

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// Providers lists the configured providers, for the startup log.
func (c Config) Providers() []string {
	var out []string
	if c.Google != nil {
		out = append(out, "google")
	}
	if c.Microsoft != nil {
		out = append(out, "microsoft")
	}
	return out
}

// Warnings returns problems that should not stop startup but need a human
// soon, e.g. a client secret expiring within SecretExpiryWarning.
func (c Config) Warnings(now time.Time) []string {
	var out []string
	if m := c.Microsoft; m != nil && !m.SecretExpiresAt.IsZero() {
		if left := m.SecretExpiresAt.Sub(now); left < SecretExpiryWarning {
			days := int(left.Hours() / 24)
			out = append(out, fmt.Sprintf(
				"MICROSOFT_CLIENT_SECRET expires in %d days (%s): create a new secret in Entra ID and update the env",
				days, m.SecretExpiresAt.Format(time.DateOnly)))
		}
	}
	return out
}

// loadOAuthClient reads <PREFIX>_CLIENT_ID, _CLIENT_SECRET and _REDIRECT_URL.
// The provider is enabled when the id or the secret is set, and then all
// three are required: a half-configured provider is an error, not a
// silently disabled one. The redirect URL alone (as in .env.example) does
// not enable it.
func loadOAuthClient(prefix string) (*OAuthClient, []error) {
	c := OAuthClient{
		ClientID:     os.Getenv(prefix + "_CLIENT_ID"),
		ClientSecret: os.Getenv(prefix + "_CLIENT_SECRET"),
		RedirectURL:  os.Getenv(prefix + "_REDIRECT_URL"),
	}
	if c.ClientID == "" && c.ClientSecret == "" {
		return nil, nil
	}

	var errs []error
	if c.ClientID == "" {
		errs = append(errs, fmt.Errorf("%s_CLIENT_ID is required when %s_CLIENT_SECRET is set", prefix, prefix))
	}
	if c.ClientSecret == "" {
		errs = append(errs, fmt.Errorf("%s_CLIENT_SECRET is required when %s_CLIENT_ID is set", prefix, prefix))
	}
	if err := validateRedirectURL(c.RedirectURL); err != nil {
		errs = append(errs, fmt.Errorf("%s_REDIRECT_URL: %w", prefix, err))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &c, nil
}

func loadMicrosoft(now time.Time) (*MicrosoftClient, []error) {
	client, errs := loadOAuthClient("MICROSOFT")
	if client == nil && len(errs) == 0 {
		return nil, nil
	}

	m := MicrosoftClient{Tenant: getEnv("MICROSOFT_TENANT_ID", "common")}
	if !validTenant(m.Tenant) {
		errs = append(errs, fmt.Errorf(
			`MICROSOFT_TENANT_ID: must be "common", "organizations", "consumers", a directory GUID or a domain, got %q`, m.Tenant))
	}

	if raw := os.Getenv("MICROSOFT_EXPIRATION_DATE"); raw != "" {
		exp, err := time.Parse(time.DateOnly, raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("MICROSOFT_EXPIRATION_DATE: must be YYYY-MM-DD, got %q", raw))
		case !now.Before(exp):
			errs = append(errs, fmt.Errorf(
				"MICROSOFT_EXPIRATION_DATE: client secret expired on %s; create a new one in Entra ID", raw))
		default:
			m.SecretExpiresAt = exp
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	m.OAuthClient = *client
	return &m, nil
}

var (
	tenantGUID   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tenantDomain = regexp.MustCompile(`^[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)+$`)
)

// validTenant accepts the authority segments Entra ID understands. It is
// interpolated into the login URL, so anything else is rejected.
func validTenant(t string) bool {
	switch strings.ToLower(t) {
	case "common", "organizations", "consumers":
		return true
	}
	return tenantGUID.MatchString(t) || tenantDomain.MatchString(t)
}

// ParseEnv validates a RELAY_ENV value. It is strict on purpose: "prod" or
// "LIVE" are typos we want to see at startup, not silently mapped to some
// mode. Exported so CLIs (relayctl) apply the exact same rule.
func ParseEnv(raw string) (apikey.Env, error) {
	switch env := apikey.Env(raw); env {
	case apikey.EnvLive, apikey.EnvTest:
		return env, nil
	case "":
		return "", errors.New(`is required ("live" or "test")`)
	default:
		return "", fmt.Errorf(`must be "live" or "test", got %q`, raw)
	}
}

func validateRedirectURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// decodeEncryptionKey never includes the value in its errors.
func decodeEncryptionKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("is required (generate one with: openssl rand -base64 32)")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("must be valid base64")
	}
	if len(key) != encryptionKeySize {
		return nil, fmt.Errorf("must decode to %d bytes, got %d", encryptionKeySize, len(key))
	}
	return key, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: must be a positive duration (e.g. 10m), got %q", key, raw)
	}
	return d, nil
}

func positiveIntEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: must be a positive integer, got %q", key, raw)
	}
	return n, nil
}
