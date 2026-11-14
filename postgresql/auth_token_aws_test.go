package postgresql

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql/driver"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/lib/pq"
)

// fakeCredentials is an aws.CredentialsProvider with a fixed expiry.
type fakeCredentials struct {
	expires time.Time
}

func (f fakeCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return aws.Credentials{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
		CanExpire:       !f.expires.IsZero(),
		Expires:         f.expires,
	}, nil
}

func newFakeRDSFetcher(t *testing.T, credsExpire time.Time) (*rdsAuthTokenFetcher, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var loads, builds atomic.Int32
	f := newRDSAuthTokenFetcher("eu-west-1", "", "", "app", "db.example.com", 5432)
	f.loadConfig = func(ctx context.Context) (aws.Config, error) {
		loads.Add(1)
		return aws.Config{Region: "eu-west-1", Credentials: fakeCredentials{expires: credsExpire}}, nil
	}
	f.buildToken = func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error) {
		builds.Add(1)
		if endpoint != "db.example.com:5432" || region != "eu-west-1" || user != "app" {
			t.Errorf("buildToken(%q, %q, %q)", endpoint, region, user)
		}
		date := time.Now().UTC().Format("20060102T150405Z")
		return "db.example.com:5432/?Action=connect&X-Amz-Date=" + date + "&X-Amz-Expires=900", nil
	}
	return f, &loads, &builds
}

func TestRDSAuthTokenFetcherCachesAWSConfig(t *testing.T) {
	f, loads, builds := newFakeRDSFetcher(t, time.Time{})

	for range 3 {
		if _, _, err := f.fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Errorf("AWS config loaded %d times for 3 tokens, want 1", got)
	}
	if got := builds.Load(); got != 3 {
		t.Errorf("token built %d times, want 3", got)
	}
}

func TestRDSAuthTokenFetcherDoesNotCacheConfigErrors(t *testing.T) {
	f, loads, _ := newFakeRDSFetcher(t, time.Time{})
	good := f.loadConfig
	f.loadConfig = func(ctx context.Context) (aws.Config, error) {
		loads.Add(1)
		return aws.Config{}, errors.New("no profile")
	}
	if _, _, err := f.fetch(context.Background()); err == nil {
		t.Fatal("fetch() expected an error")
	}
	f.loadConfig = good
	if _, _, err := f.fetch(context.Background()); err != nil {
		t.Fatalf("fetch() after a failed config load: %v", err)
	}
	if got := loads.Load(); got != 2 {
		t.Errorf("AWS config loaded %d times, want 2", got)
	}
}

func TestRDSAuthTokenFetcherExpiry(t *testing.T) {
	// Without expiring credentials the token's own expiry (15 min) applies.
	f, _, _ := newFakeRDSFetcher(t, time.Time{})
	_, expiresAt, err := f.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expiresAt); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("token expires in %v, want about 15 minutes", d)
	}

	// Credentials that expire first cap the token's lifetime.
	soon := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	f, _, _ = newFakeRDSFetcher(t, soon)
	_, expiresAt, err = f.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(soon) {
		t.Errorf("token expires at %v, want the credentials' expiry %v", expiresAt, soon)
	}
}

func TestRDSAuthTokenFetcherAssumeRoleKeepsRegion(t *testing.T) {
	f := newRDSAuthTokenFetcher("eu-west-1", "", "arn:aws:iam::123456789012:role/db", "app", "db", 5432)
	f.loadConfig = func(ctx context.Context) (aws.Config, error) {
		return aws.Config{Region: "eu-west-1", Credentials: fakeCredentials{}}, nil
	}
	// No STS call happens until the credentials are retrieved.
	cfg, err := f.config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu-west-1" {
		t.Errorf("region = %q after assuming the role, want eu-west-1", cfg.Region)
	}
	if _, ok := cfg.Credentials.(*aws.CredentialsCache); !ok {
		t.Errorf("assumed role credentials = %T, want a refreshing *aws.CredentialsCache", cfg.Credentials)
	}
}

func TestAWSPostgresConfig(t *testing.T) {
	cfg, err := awsPostgresConfig("awspostgres://app:@db.example.com:5432/mydb?fallback_application_name=tf")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.com" || cfg.Port != 5432 || cfg.Database != "mydb" || cfg.User != "app" {
		t.Errorf("connection parameters lost: %+v", cfg)
	}
	if cfg.SSLMode != pq.SSLModeDisable {
		t.Errorf("sslmode = %q, want disable (the dialer provides TLS)", cfg.SSLMode)
	}

	for _, param := range []string{"sslmode=require", "sslrootcert=/ca.pem", "sslcert=/c.pem", "sslkey=/k.pem"} {
		if _, err := awsPostgresConfig("awspostgres://app:@db:5432/mydb?" + param); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: err = %v, want a 'not allowed' error like gocloud's", param, err)
		}
	}
}

func TestNewPostgresConnectorAWSPostgres(t *testing.T) {
	c := &Config{
		Scheme: "awspostgres",
		AuthTokens: newAuthTokenSource("AWS RDS IAM", func(ctx context.Context) (string, time.Time, error) {
			return "token", time.Now().Add(time.Hour), nil
		}),
	}
	connector, err := c.newPostgresConnector("awspostgres://app:@db.example.com:5432/mydb")
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := connector.(*tokenAuthConnector)
	if !ok {
		t.Fatalf("connector = %T, want *tokenAuthConnector", connector)
	}
	if _, ok := tc.dialer.(rdsTLSDialer); !ok {
		t.Errorf("dialer = %T, want rdsTLSDialer", tc.dialer)
	}
	var password string
	tc.open = func(ctx context.Context, cfg pq.Config, d pq.Dialer) (driver.Conn, error) {
		password = cfg.Password
		return nil, errTestDial
	}
	_, _ = tc.Connect(context.Background())
	if password != "token" {
		t.Errorf("password = %q, want the token", password)
	}
}

// staticCertPool is an rds.CertPoolProvider returning a fixed pool.
type staticCertPool struct {
	pool *x509.CertPool
}

func (p staticCertPool) RDSCertPool(ctx context.Context) (*x509.CertPool, error) {
	return p.pool, nil
}

// newTestTLSServer starts a listener that answers the PostgreSQL SSLRequest
// with reply and, for 'S', completes a TLS handshake with a certificate for
// 127.0.0.1 / localhost. It returns the address and a pool trusting the cert.
func newTestTLSServer(t *testing.T, reply byte) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var req [8]byte
				if _, err := io.ReadFull(conn, req[:]); err != nil {
					return
				}
				if _, err := conn.Write([]byte{reply}); err != nil || reply != 'S' {
					return
				}
				srv := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
				})
				if err := srv.Handshake(); err != nil {
					return
				}
				_, _ = srv.Write([]byte("ok"))
			}()
		}
	}()
	return ln.Addr().String(), pool
}

func TestRDSTLSDialer(t *testing.T) {
	addr, pool := newTestTLSServer(t, 'S')

	conn, err := rdsTLSDialer{certs: staticCertPool{pool}}.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("DialTimeout() with the server's CA: %v", err)
	}
	var buf [2]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil || string(buf[:]) != "ok" {
		t.Errorf("read over TLS = %q, %v", buf, err)
	}
	_ = conn.Close()

	// An untrusted certificate fails the handshake.
	if _, err := (rdsTLSDialer{certs: staticCertPool{x509.NewCertPool()}}).Dial("tcp", addr); err == nil {
		t.Error("Dial() with an untrusted certificate expected an error")
	}
}

func TestRDSTLSDialerSSLNotSupported(t *testing.T) {
	addr, pool := newTestTLSServer(t, 'N')
	if _, err := (rdsTLSDialer{certs: staticCertPool{pool}}).Dial("tcp", addr); !errors.Is(err, pq.ErrSSLNotSupported) {
		t.Errorf("Dial() = %v, want pq.ErrSSLNotSupported", err)
	}
}
