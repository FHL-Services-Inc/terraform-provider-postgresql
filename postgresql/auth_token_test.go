package postgresql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
)

// fakeClock is a manually advanced clock for authTokenSource tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeTokenFetcher hands out "token-1", "token-2", ... valid for ttl, and
// counts how often it was called.
type fakeTokenFetcher struct {
	clock *fakeClock
	ttl   time.Duration
	calls atomic.Int32
	err   error
	delay time.Duration
}

func (f *fakeTokenFetcher) fetch(ctx context.Context) (string, time.Time, error) {
	n := f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return fmt.Sprintf("token-%d", n), f.clock.Now().Add(f.ttl), nil
}

func newTestTokenSource(f *fakeTokenFetcher) *authTokenSource {
	s := newAuthTokenSource("test", f.fetch)
	s.now = f.clock.Now
	return s
}

func TestAuthTokenSourceReusesTokenUntilRefreshPoint(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: 15 * time.Minute}
	s := newTestTokenSource(f)

	tok, err := s.Token(context.Background())
	if err != nil || tok != "token-1" {
		t.Fatalf("Token() = %q, %v; want token-1", tok, err)
	}

	// Refresh happens at 80% of the TTL (12 min for a 15 min token).
	clock.Advance(11*time.Minute + 59*time.Second)
	if tok, _ := s.Token(context.Background()); tok != "token-1" {
		t.Errorf("before the refresh point Token() = %q, want cached token-1", tok)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetch called %d times before the refresh point, want 1", got)
	}

	clock.Advance(time.Second)
	if tok, _ := s.Token(context.Background()); tok != "token-2" {
		t.Errorf("at the refresh point Token() = %q, want fresh token-2", tok)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetch called %d times after the refresh point, want 2", got)
	}
}

func TestAuthTokenSourceRefetchesAfterExpiry(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: time.Hour}
	s := newTestTokenSource(f)

	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if tok, _ := s.Token(context.Background()); tok != "token-2" {
		t.Errorf("after expiry Token() = %q, want token-2", tok)
	}
}

func TestAuthTokenSourceConcurrentCallersFetchOnce(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: 15 * time.Minute, delay: 50 * time.Millisecond}
	s := newTestTokenSource(f)

	const callers = 32
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			tokens[i], errs[i] = s.Token(context.Background())
		})
	}
	wg.Wait()

	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetch called %d times by %d concurrent callers, want 1", got, callers)
	}
	for i := range callers {
		if errs[i] != nil || tokens[i] != "token-1" {
			t.Errorf("caller %d got %q, %v; want token-1", i, tokens[i], errs[i])
		}
	}
}

func TestAuthTokenSourceDoesNotCacheErrors(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: 15 * time.Minute, err: errors.New("sts unavailable")}
	s := newTestTokenSource(f)

	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("Token() expected an error")
	}
	f.err = nil
	tok, err := s.Token(context.Background())
	if err != nil || tok != "token-2" {
		t.Fatalf("Token() after a failed fetch = %q, %v; want token-2", tok, err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetch called %d times, want 2 (the error must not be cached)", got)
	}
}

func TestAuthTokenSourceFallsBackToUnexpiredTokenOnRefreshError(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: 15 * time.Minute}
	s := newTestTokenSource(f)

	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Past the refresh point but before expiry: a failed refresh keeps
	// serving the still valid token.
	clock.Advance(13 * time.Minute)
	f.err = errors.New("throttled")
	tok, err := s.Token(context.Background())
	if err != nil || tok != "token-1" {
		t.Fatalf("Token() with a failing refresh = %q, %v; want token-1", tok, err)
	}

	// Once the token has expired the error surfaces.
	clock.Advance(3 * time.Minute)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("Token() with an expired token and a failing refresh expected an error")
	}
}

func TestAuthTokenSourceWaiterHonorsContext(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	release := make(chan struct{})
	started := make(chan struct{})
	s := newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		close(started)
		<-release
		return "token", clock.Now().Add(time.Hour), nil
	})
	s.now = clock.Now

	go func() { _, _ = s.Token(context.Background()) }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting Token() = %v, want context.DeadlineExceeded", err)
	}
	close(release)
}

func TestAuthTokenSourcePassesContextToFetch(t *testing.T) {
	type ctxKey struct{}
	var got any
	s := newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		got = ctx.Value(ctxKey{})
		return "token", time.Now().Add(time.Hour), nil
	})
	if _, err := s.Token(context.WithValue(context.Background(), ctxKey{}, "dial")); err != nil {
		t.Fatal(err)
	}
	if got != "dial" {
		t.Errorf("fetch got context value %v, want the caller's context", got)
	}
}

func TestRDSAuthTokenExpiry(t *testing.T) {
	issued := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	fallback := issued.Add(defaultRDSAuthTokenTTL)

	for _, tc := range []struct {
		name  string
		token string
		want  time.Time
	}{
		{
			name:  "presigned",
			token: "db.example.com:5432/?Action=connect&DBUser=app&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20260101%2Fus-east-1%2Frds-db%2Faws4_request&X-Amz-Date=20260101T100000Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=abc",
			want:  issued.Add(15 * time.Minute),
		},
		{
			name:  "short expiry",
			token: "db.example.com:5432/?Action=connect&X-Amz-Date=20260101T100000Z&X-Amz-Expires=300",
			want:  issued.Add(5 * time.Minute),
		},
		{
			name:  "missing parameters",
			token: "db.example.com:5432/?Action=connect",
			want:  fallback,
		},
		{
			name:  "garbage",
			token: "%%%",
			want:  fallback,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rdsAuthTokenExpiry(tc.token, issued); !got.Equal(tc.want) {
				t.Errorf("rdsAuthTokenExpiry() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRedactTokenError(t *testing.T) {
	const token = "db.example.com:5432/?Action=connect&X-Amz-Signature=s3cr3t"

	pqErr := &pq.Error{Code: "28P01", Message: "password authentication failed"}
	if got := redactTokenError(pqErr, token); got != pqErr {
		t.Errorf("a *pq.Error must be returned unchanged, got %v", got)
	}

	plain := errors.New("dial tcp: connection refused")
	if got := redactTokenError(plain, token); got != plain {
		t.Errorf("an error without the token must be returned unchanged, got %v", got)
	}

	for _, leaked := range []error{
		fmt.Errorf("bad conn: %s", token),
		fmt.Errorf("bad conn: %s", url.QueryEscape(token)),
	} {
		got := redactTokenError(leaked, token)
		if strings.Contains(got.Error(), "s3cr3t") {
			t.Errorf("redacted error still contains the token: %v", got)
		}
		if !strings.Contains(got.Error(), "XXXX") {
			t.Errorf("redacted error = %q, want an XXXX placeholder", got)
		}
		if errors.Unwrap(got) != nil {
			t.Errorf("redacted error must not wrap the original (its message holds the token)")
		}
	}
}

// recordingDialer fails every dial, recording that it was used.
type recordingDialer struct {
	dials atomic.Int32
}

var errTestDial = errors.New("test dialer: no network")

func (d *recordingDialer) Dial(network, address string) (net.Conn, error) {
	d.dials.Add(1)
	return nil, errTestDial
}

func (d *recordingDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	d.dials.Add(1)
	return nil, errTestDial
}

func TestTokenAuthConnectorFetchesTokenPerConnect(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeTokenFetcher{clock: clock, ttl: 15 * time.Minute}
	tokens := newTestTokenSource(f)

	c, err := newTokenAuthConnector("postgres://app:@db.example.com:5432/postgres?sslmode=require&connect_timeout=7", tokens, proxyDriver{})
	if err != nil {
		t.Fatal(err)
	}
	var passwords []string
	c.open = func(ctx context.Context, cfg pq.Config, d pq.Dialer) (driver.Conn, error) {
		passwords = append(passwords, cfg.Password)
		if cfg.Host != "db.example.com" || cfg.Port != 5432 || cfg.User != "app" ||
			cfg.SSLMode != pq.SSLModeRequire || cfg.ConnectTimeout != 7*time.Second {
			t.Errorf("connection parameters lost: %+v", cfg)
		}
		if _, ok := d.(proxyDriver); !ok {
			t.Errorf("dialer = %T, want proxyDriver (proxy support)", d)
		}
		return nil, errTestDial
	}

	for range 2 {
		_, _ = c.Connect(context.Background())
	}
	clock.Advance(13 * time.Minute)
	_, _ = c.Connect(context.Background())

	want := []string{"token-1", "token-1", "token-2"}
	if strings.Join(passwords, ",") != strings.Join(want, ",") {
		t.Errorf("passwords used = %v, want %v", passwords, want)
	}
	// A connect sets the token on a clone, never on the parsed config (which
	// may hold PGPASSWORD from the environment; it is always overridden).
	if strings.HasPrefix(c.cfg.Password, "token-") {
		t.Errorf("base config password = %q, a token leaked into the shared config", c.cfg.Password)
	}
}

func TestTokenAuthConnectorRedactsTokenInErrors(t *testing.T) {
	const secret = "db.example.com:5432/?Action=connect&X-Amz-Signature=s3cr3t"
	tokens := newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		return secret, time.Now().Add(time.Hour), nil
	})
	c, err := newTokenAuthConnector("postgres://app:@db.example.com:5432/postgres", tokens, proxyDriver{})
	if err != nil {
		t.Fatal(err)
	}
	c.open = func(ctx context.Context, cfg pq.Config, d pq.Dialer) (driver.Conn, error) {
		return nil, fmt.Errorf("handshake failed for password %s", cfg.Password)
	}

	_, err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect() expected an error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("Connect() error leaks the token: %v", err)
	}
}

func TestTokenAuthConnectorTokenError(t *testing.T) {
	tokens := newAuthTokenSource("AWS RDS IAM", func(ctx context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("no credentials")
	})
	c, err := newTokenAuthConnector("postgres://app:@db.example.com:5432/postgres", tokens, proxyDriver{})
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	c.open = func(ctx context.Context, cfg pq.Config, d pq.Dialer) (driver.Conn, error) {
		opened = true
		return nil, nil
	}
	_, err = c.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no credentials") || !strings.Contains(err.Error(), "AWS RDS IAM") {
		t.Errorf("Connect() error = %v, want the token source error", err)
	}
	if opened {
		t.Error("Connect() must not dial without a token")
	}
}

// TestTokenAuthConnectorUsesDialer runs the real pq connect path with a
// dialer that refuses to dial, to check the dialer (proxy support) is wired in.
func TestTokenAuthConnectorUsesDialer(t *testing.T) {
	tokens := newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		return "token", time.Now().Add(time.Hour), nil
	})
	d := &recordingDialer{}
	c, err := newTokenAuthConnector("postgres://app:@db.example.com:5432/postgres?sslmode=disable", tokens, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background()); !errors.Is(err, errTestDial) {
		t.Errorf("Connect() = %v, want the dialer's error", err)
	}
	if d.dials.Load() == 0 {
		t.Error("the configured dialer was not used")
	}
	if _, ok := c.Driver().(*pq.Driver); !ok {
		t.Errorf("Driver() = %T, want *pq.Driver", c.Driver())
	}
}

func TestConnStrWithTokenSourceHasNoPassword(t *testing.T) {
	tokens := newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		return "s3cr3t-token", time.Now().Add(time.Hour), nil
	})
	if _, err := tokens.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := Config{Scheme: "postgres", Host: "db", Port: 5432, Username: "app", SSLMode: "require", AuthTokens: tokens}
	dsn := c.connStr("postgres")
	if strings.Contains(dsn, "s3cr3t") {
		t.Errorf("connStr() = %q leaks the token into the registry key", dsn)
	}
	if dsn != c.connStr("postgres") {
		t.Error("connStr() must be stable for the token path")
	}
}

func TestConnectWithRetryRedactionWithEmptyPassword(t *testing.T) {
	// An empty Password must not turn the redaction into an insertion of
	// XXXX (strings.Replace with an empty old string inserts).
	drv := &flakyPingDriver{failuresBeforeSuccess: 100}
	c := newFlakyClient("flaky-empty-password", drv, 0, 5)
	_, err := c.connectWithRetry(context.Background(), "flaky-empty-password", "dsn")
	if err == nil {
		t.Fatal("connectWithRetry() expected an error")
	}
	if strings.Contains(err.Error(), "XXXX") {
		t.Errorf("error = %q, empty password must not be redacted", err)
	}
}

// TestAccTokenAuthConnectorRefreshesToken checks the token path end to end
// against a real server, with PGPASSWORD standing in for an IAM token: every
// physical connection authenticates with a token from the source, concurrent
// dials share one fetch, and the token is refreshed once it reaches its
// refresh point.
func TestAccTokenAuthConnectorRefreshesToken(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	password := config.Password
	if password == "" {
		t.Skip("PGPASSWORD is required to stand in for an auth token")
	}

	const ttl = 3 * time.Second
	var fetches atomic.Int32
	config.Password = ""
	config.AuthTokens = newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		fetches.Add(1)
		return password, time.Now().Add(ttl), nil
	})
	config.MaxConns = 8
	config.MaxIdleConns = 0
	config.ConnectionRetryTimeoutSeconds = defaultProviderConnectionRetryTimeoutSeconds

	// The auth kind in the registry key keeps this pool apart from the ones
	// other tests register for the same database.
	dsn := config.connStr("postgres")
	if strings.Contains(dsn, ":"+url.PathEscape(password)+"@") {
		t.Fatalf("connStr() = %q contains the token", dsn)
	}
	db, err := config.NewClient("postgres").Connect()
	if err != nil {
		t.Fatalf("Connect() with a token source: %v", err)
	}
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, config.registryKey("postgres"))
		dbRegistryLock.Unlock()
		_ = db.Close()
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			var one int
			if err := db.QueryRow("SELECT 1 FROM pg_sleep(0.05)").Scan(&one); err != nil {
				t.Errorf("query over a token-authenticated connection: %v", err)
			}
		})
	}
	wg.Wait()
	if got := fetches.Load(); got != 1 {
		t.Errorf("token fetched %d times within its lifetime, want 1", got)
	}

	// With max_idle_connections = 0 every query dials again; past the
	// refresh point that dial has to fetch a new token.
	time.Sleep(ttl)
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping() after the token's refresh point: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Errorf("token fetched %d times after its refresh point, want 2", got)
	}
}
