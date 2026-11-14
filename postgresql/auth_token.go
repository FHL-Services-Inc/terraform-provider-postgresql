package postgresql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	// authTokenRefreshFraction is the share of a token's lifetime after which
	// it is replaced: a 15-minute RDS token is refreshed after 12 minutes, so
	// a connection is never opened with a token that is about to expire.
	authTokenRefreshFraction = 0.8

	// defaultRDSAuthTokenTTL is the lifetime assumed for an RDS IAM token whose
	// expiry can't be read from its X-Amz-Date / X-Amz-Expires parameters.
	// RDS tokens are valid for 15 minutes; stay well below that.
	defaultRDSAuthTokenTTL = 10 * time.Minute
)

// authTokenFetcher obtains a new auth token and the time it expires.
type authTokenFetcher func(ctx context.Context) (token string, expiresAt time.Time, err error)

// authTokenSource caches a short-lived auth token (AWS RDS IAM, Azure AD) and
// refreshes it once authTokenRefreshFraction of its lifetime has passed.
//
// It is shared by every connection pool of a provider instance and called for
// each new physical connection, so only one caller fetches at a time; the
// others wait for its result instead of hitting STS/IMDS/Entra in parallel.
// Failed fetches are not cached.
type authTokenSource struct {
	// name describes the token in errors and logs ("AWS RDS IAM").
	name  string
	fetch authTokenFetcher
	now   func() time.Time

	// sem is a one-slot semaphore guarding the fields below. It is a channel
	// rather than a mutex so that callers waiting for a fetch give up when
	// their context is done.
	sem       chan struct{}
	token     string
	refreshAt time.Time
	expiresAt time.Time
}

func newAuthTokenSource(name string, fetch authTokenFetcher) *authTokenSource {
	return &authTokenSource{
		name:  name,
		fetch: fetch,
		now:   time.Now,
		sem:   make(chan struct{}, 1),
	}
}

// Token returns a cached token, or fetches a new one if there is none or the
// cached one reached its refresh point. ctx bounds the wait and the fetch.
func (s *authTokenSource) Token(ctx context.Context) (string, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.sem }()

	now := s.now()
	if s.token != "" && now.Before(s.refreshAt) {
		return s.token, nil
	}

	token, expiresAt, err := s.fetch(ctx)
	if err != nil {
		if s.token != "" && now.Before(s.expiresAt) {
			log.Printf("[WARN] could not refresh %s auth token, using the current one (valid until %s): %v",
				s.name, s.expiresAt.Format(time.RFC3339), err)
			return s.token, nil
		}
		return "", err
	}

	s.token = token
	s.expiresAt = expiresAt
	s.refreshAt = now.Add(time.Duration(float64(expiresAt.Sub(now)) * authTokenRefreshFraction))
	log.Printf("[DEBUG] obtained %s auth token, valid until %s", s.name, expiresAt.Format(time.RFC3339))
	return token, nil
}

// rdsAuthTokenExpiry returns when an RDS IAM token, issued at issuedAt,
// expires. The token is a presigned URL without scheme; its expiry is
// X-Amz-Date + X-Amz-Expires. Falls back to issuedAt + defaultRDSAuthTokenTTL
// if those can't be parsed.
func rdsAuthTokenExpiry(token string, issuedAt time.Time) time.Time {
	fallback := issuedAt.Add(defaultRDSAuthTokenTTL)

	u, err := url.Parse("https://" + token)
	if err != nil {
		return fallback
	}
	q := u.Query()
	signed, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return fallback
	}
	seconds, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || seconds <= 0 {
		return fallback
	}
	return signed.Add(time.Duration(seconds) * time.Second)
}

// redactTokenError returns err with any occurrence of token replaced by XXXX.
// Errors that don't contain the token are returned unchanged, so callers can
// still inspect them (e.g. *pq.Error for SQLSTATE checks). A redacted error
// does not wrap the original, whose message holds the token.
func redactTokenError(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	redacted := msg
	for _, form := range []string{token, url.PathEscape(token), url.QueryEscape(token)} {
		redacted = strings.ReplaceAll(redacted, form, "XXXX")
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

// tokenAuthConnector is a driver.Connector for the "postgres" scheme that
// authenticates every new physical connection with a token from an
// authTokenSource instead of a password fixed in the DSN. The DSN is parsed
// once; each Connect clones the parsed config and sets the current token as
// the password.
type tokenAuthConnector struct {
	cfg    pq.Config
	tokens *authTokenSource
	dialer pq.Dialer

	// open dials with the final config (overridable by tests).
	open func(ctx context.Context, cfg pq.Config, dialer pq.Dialer) (driver.Conn, error)
}

func newTokenAuthConnector(dsn string, tokens *authTokenSource, dialer pq.Dialer) (*tokenAuthConnector, error) {
	cfg, err := pq.NewConfig(dsn)
	if err != nil {
		return nil, err
	}
	return &tokenAuthConnector{
		cfg:    cfg,
		tokens: tokens,
		dialer: dialer,
		open:   openPQConn,
	}, nil
}

func openPQConn(ctx context.Context, cfg pq.Config, dialer pq.Dialer) (driver.Conn, error) {
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector.Dialer(dialer)
	conn, err := connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Connect implements driver.Connector. The token is redacted from any error
// here, where it is known: the generic redaction in connectWithRetry only
// knows the static Config.Password, which is empty on this path.
func (c *tokenAuthConnector) Connect(ctx context.Context) (driver.Conn, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get %s auth token: %w", c.tokens.name, err)
	}

	cfg := c.cfg.Clone()
	cfg.Password = token
	conn, err := c.open(ctx, cfg, c.dialer)
	if err != nil {
		return nil, redactTokenError(err, token)
	}
	return conn, nil
}

// Driver implements driver.Connector.
func (c *tokenAuthConnector) Driver() driver.Driver { return &pq.Driver{} }

// newPostgresConnector returns the connector used for the "postgres" scheme
// when an auth token source is configured: connections are dialed through
// proxyDriver (honoring ALL_PROXY / HTTPS_PROXY) and authenticated with a
// fresh token.
func (c *Config) newPostgresConnector(dsn string) (driver.Connector, error) {
	return newTokenAuthConnector(dsn, c.AuthTokens, proxyDriver{})
}
