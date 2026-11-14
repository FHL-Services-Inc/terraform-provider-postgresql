package postgresql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/lib/pq"
	"gocloud.dev/aws/rds"
)

// rdsCredentialsExpiryWindow is how long before their expiry assumed role
// credentials are renewed.
const rdsCredentialsExpiryWindow = 5 * time.Minute

// rdsAuthTokenFetcher builds RDS IAM auth tokens. The AWS config (and, with
// a role ARN, the assumed role credentials) is loaded on the first fetch and
// reused: its credentials provider is an aws.CredentialsCache that renews the
// credentials when they expire, so a token refresh is a local signing
// operation. A failed load isn't kept; the next fetch loads again.
//
// fetch is only called through authTokenSource, which serializes calls, so
// cfg needs no lock of its own.
type rdsAuthTokenFetcher struct {
	region, profile, role string
	username              string
	endpoint              string

	cfg *aws.Config

	// Overridable by tests.
	loadConfig func(ctx context.Context) (aws.Config, error)
	buildToken func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error)
}

func newRDSAuthTokenFetcher(region, profile, role, username, host string, port int) *rdsAuthTokenFetcher {
	f := &rdsAuthTokenFetcher{
		region:   region,
		profile:  profile,
		role:     role,
		username: username,
		endpoint: fmt.Sprintf("%s:%d", host, port),
	}
	f.buildToken = func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error) {
		return auth.BuildAuthToken(ctx, endpoint, region, user, creds)
	}
	f.loadConfig = f.loadDefaultConfig
	return f
}

func (f *rdsAuthTokenFetcher) loadDefaultConfig(ctx context.Context) (aws.Config, error) {
	switch {
	case f.profile != "":
		return awsConfig.LoadDefaultConfig(ctx, awsConfig.WithSharedConfigProfile(f.profile))
	case f.region != "":
		return awsConfig.LoadDefaultConfig(ctx, awsConfig.WithRegion(f.region))
	default:
		return awsConfig.LoadDefaultConfig(ctx)
	}
}

// config returns the cached AWS config, loading it on first use.
func (f *rdsAuthTokenFetcher) config(ctx context.Context) (aws.Config, error) {
	if f.cfg != nil {
		return *f.cfg, nil
	}
	cfg, err := f.loadConfig(ctx)
	if err != nil {
		return aws.Config{}, err
	}
	if f.role != "" {
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), f.role, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "TerraformPostgresqlProvider"
			// The STS default, as with the plain AssumeRole call this
			// replaces (stscreds would otherwise ask for 15 minutes).
			o.Duration = time.Hour
		})
		cfg = cfg.Copy()
		cfg.Credentials = aws.NewCredentialsCache(provider, func(o *aws.CredentialsCacheOptions) {
			// Renew the session before it gets close to its end, so a token
			// isn't signed with credentials about to expire.
			o.ExpiryWindow = rdsCredentialsExpiryWindow
		})
	}
	f.cfg = &cfg
	return cfg, nil
}

// fetch builds a token and returns it with its expiry: the presigned URL's
// own expiry, or earlier if the signing credentials expire first (the token
// stops working with them).
func (f *rdsAuthTokenFetcher) fetch(ctx context.Context) (string, time.Time, error) {
	cfg, err := f.config(ctx)
	if err != nil {
		return "", time.Time{}, err
	}

	issuedAt := time.Now()
	token, err := f.buildToken(ctx, f.endpoint, cfg.Region, f.username, cfg.Credentials)
	if err != nil {
		if f.role != "" {
			return "", time.Time{}, fmt.Errorf("could not build RDS auth token with AWS role %s: %w", f.role, err)
		}
		return "", time.Time{}, err
	}
	expiresAt := rdsAuthTokenExpiry(token, issuedAt)
	// buildToken retrieved the credentials through the cache, so this
	// doesn't call out again.
	if creds, err := cfg.Credentials.Retrieve(ctx); err == nil && creds.CanExpire && creds.Expires.Before(expiresAt) {
		expiresAt = creds.Expires
	}
	return token, expiresAt, nil
}

// awsPostgresConfig parses an awspostgres:// DSN the way gocloud's
// awspostgres opener does: TLS comes from rdsTLSDialer, so SSL parameters
// are rejected and pq's own TLS is disabled.
func awsPostgresConfig(dsn string) (pq.Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return pq.Config{}, err
	}
	query := u.Query()
	for k := range query {
		if k == "sslmode" || k == "sslcert" || k == "sslkey" || k == "sslrootcert" {
			return pq.Config{}, fmt.Errorf("awspostgres: open: parameter %q not allowed; sslmode must be disabled because the underlying dialer is already providing TLS", k)
		}
	}
	query.Set("sslmode", "disable")

	u2 := *u
	u2.Scheme = "postgres"
	u2.RawQuery = query.Encode()
	return pq.NewConfig(u2.String())
}

// newAWSPostgresConnector returns a connector for an awspostgres:// DSN with
// a static password, dialing through dialer (rdsTLSDialer): what gocloud's
// awspostgres opener does, but with the cached RDS CA bundle.
func newAWSPostgresConnector(dsn string, dialer pq.Dialer) (driver.Connector, error) {
	cfg, err := awsPostgresConfig(dsn)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector.Dialer(dialer)
	return connector, nil
}

// openAWSPostgres opens a pool for the "awspostgres" scheme with password
// authentication (with auth tokens, see newPostgresConnector).
func (c *Config) openAWSPostgres(dsn string) (*sql.DB, error) {
	connector, err := newAWSPostgresConnector(dsn, newRDSTLSDialer())
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

// rdsTLSDialer is the dialer of gocloud's awspostgres, which keeps it
// private: it sends the PostgreSQL SSLRequest itself and upgrades the
// connection to TLS, verifying the server against the RDS CA bundle.
// Like gocloud's, it dials directly (no proxy). Unlike gocloud's, the
// provider's dialer takes the bundle from defaultRDSCertPool instead of
// downloading it on each dial; if the server's CA isn't in the cached
// bundle, the bundle is refetched and the dial retried once.
type rdsTLSDialer struct {
	certs rds.CertPoolProvider
}

// certPoolInvalidator is implemented by rdsCertPoolCache.
type certPoolInvalidator interface {
	invalidate(pool *x509.CertPool) bool
}

func newRDSTLSDialer() rdsTLSDialer {
	return rdsTLSDialer{certs: defaultRDSCertPool}
}

func (d rdsTLSDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("awspostgres: parse address: %w", err)
	}
	for retried := false; ; retried = true {
		certPool, err := d.certs.RDSCertPool(ctx)
		if err != nil {
			return nil, err
		}
		conn, err := dialRDSTLS(ctx, network, address, host, certPool)
		if err == nil {
			return conn, nil
		}

		var unknownAuthority x509.UnknownAuthorityError
		invalidator, ok := d.certs.(certPoolInvalidator)
		if retried || !ok || !errors.As(err, &unknownAuthority) || !invalidator.invalidate(certPool) {
			return nil, err
		}
		log.Printf("[DEBUG] %s: server certificate signed by a CA unknown to the cached RDS CA bundle, refetching it", address)
	}
}

// dialRDSTLS opens a TLS connection to a PostgreSQL server, verified against
// certPool.
func dialRDSTLS(ctx context.Context, network, address, host string, certPool *x509.CertPool) (net.Conn, error) {
	conn, err := new(net.Dialer).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}

	// SSLRequest: message length 8 (Int32), then the magic number 80877103.
	if _, err := conn.Write([]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The server must answer 'S'.
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reply[0] != 'S' {
		_ = conn.Close()
		return nil, pq.ErrSSLNotSupported
	}

	crypt := tls.Client(conn, &tls.Config{
		ServerName:    host,
		RootCAs:       certPool,
		Renegotiation: tls.RenegotiateFreelyAsClient,
	})
	if err := crypt.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return crypt, nil
}

// DialContext lets pq pass the connect context (pq.DialerContext).
func (d rdsTLSDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

func (d rdsTLSDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(context.Background(), network, address)
}

func (d rdsTLSDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.dial(ctx, network, address)
}
