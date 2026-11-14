package postgresql

import (
	"context"
	"crypto/x509"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCertFetcher is an rds.CertPoolProvider returning pools[min(n, len-1)]
// on the n-th call (or err), counting calls.
type fakeCertFetcher struct {
	mu    sync.Mutex
	pools []*x509.CertPool
	err   error
	calls atomic.Int32
	delay time.Duration
}

func (f *fakeCertFetcher) RDSCertPool(ctx context.Context) (*x509.CertPool, error) {
	n := int(f.calls.Add(1)) - 1
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.pools[min(n, len(f.pools)-1)], nil
}

func (f *fakeCertFetcher) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func newTestCertCache(f *fakeCertFetcher, clock *fakeClock) *rdsCertPoolCache {
	c := newRDSCertPoolCache(f)
	c.now = clock.Now
	return c
}

func TestRDSCertPoolCacheConcurrentDialsFetchOnce(t *testing.T) {
	addr, pool := newTestTLSServer(t, 'S')
	f := &fakeCertFetcher{pools: []*x509.CertPool{pool}, delay: 20 * time.Millisecond}
	cache := newRDSCertPoolCache(f)
	d := rdsTLSDialer{certs: cache}

	const dials = 16
	var wg sync.WaitGroup
	for range dials {
		wg.Go(func() {
			conn, err := d.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Errorf("DialTimeout(): %v", err)
				return
			}
			_ = conn.Close()
		})
	}
	wg.Wait()
	if got := f.calls.Load(); got != 1 {
		t.Errorf("CA bundle fetched %d times for %d dials, want 1", got, dials)
	}
}

func TestRDSCertPoolCacheRefreshesAfterTTL(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, second := x509.NewCertPool(), x509.NewCertPool()
	f := &fakeCertFetcher{pools: []*x509.CertPool{first, second}}
	cache := newTestCertCache(f, clock)

	if p, err := cache.RDSCertPool(context.Background()); err != nil || p != first {
		t.Fatalf("RDSCertPool() = %p, %v; want the first pool", p, err)
	}
	clock.Advance(rdsCertPoolTTL - time.Second)
	if p, _ := cache.RDSCertPool(context.Background()); p != first {
		t.Error("pool refetched before its TTL")
	}
	clock.Advance(time.Second)
	if p, _ := cache.RDSCertPool(context.Background()); p != second {
		t.Error("pool not refetched after its TTL")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2", got)
	}
}

func TestRDSCertPoolCacheFailsClosedWithoutBundle(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := &fakeCertFetcher{err: errors.New("truststore unreachable")}
	cache := newTestCertCache(f, clock)

	if p, err := cache.RDSCertPool(context.Background()); err == nil || p != nil {
		t.Fatalf("RDSCertPool() = %v, %v; want an error and no pool", p, err)
	}
	// Errors aren't cached.
	f.setErr(nil)
	f.pools = []*x509.CertPool{x509.NewCertPool()}
	if _, err := cache.RDSCertPool(context.Background()); err != nil {
		t.Fatalf("RDSCertPool() after a failed fetch: %v", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2", got)
	}
}

func TestRDSCertPoolCacheKeepsStalePoolOnRefreshError(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	pool := x509.NewCertPool()
	f := &fakeCertFetcher{pools: []*x509.CertPool{pool}}
	cache := newTestCertCache(f, clock)

	if _, err := cache.RDSCertPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(rdsCertPoolTTL + time.Minute)
	f.setErr(errors.New("truststore unreachable"))
	// The previous bundle still verifies servers (nothing is skipped), so it
	// keeps being used until a refresh succeeds.
	if p, err := cache.RDSCertPool(context.Background()); err != nil || p != pool {
		t.Errorf("RDSCertPool() with a failing refresh = %p, %v; want the previous pool", p, err)
	}
}

func TestRDSTLSDialerRefetchesOnUnknownAuthority(t *testing.T) {
	addr, pool := newTestTLSServer(t, 'S')
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// The cached bundle predates the server's CA; the next fetch has it.
	f := &fakeCertFetcher{pools: []*x509.CertPool{x509.NewCertPool(), pool}}
	cache := newTestCertCache(f, clock)
	if _, err := cache.RDSCertPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(rdsCertPoolMinRefetchInterval)

	conn, err := rdsTLSDialer{certs: cache}.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("DialTimeout() after an unknown authority: %v", err)
	}
	_ = conn.Close()
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2 (one refetch on unknown authority)", got)
	}
}

func TestRDSTLSDialerUnknownAuthorityFailsClosed(t *testing.T) {
	addr, _ := newTestTLSServer(t, 'S')
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// The server's CA is never in the bundle.
	f := &fakeCertFetcher{pools: []*x509.CertPool{x509.NewCertPool()}}
	cache := newTestCertCache(f, clock)
	d := rdsTLSDialer{certs: cache}

	var unknown x509.UnknownAuthorityError
	if _, err := d.Dial("tcp", addr); !errors.As(err, &unknown) {
		t.Fatalf("Dial() = %v, want an unknown authority error", err)
	}
	// A bundle fetched moments ago isn't fetched again for every failing
	// dial: a misconfigured server must not turn into a download per dial.
	for range 3 {
		if _, err := d.Dial("tcp", addr); err == nil {
			t.Fatal("Dial() to an untrusted server succeeded")
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetched %d times, want 1 (bundle younger than the minimum refetch interval)", got)
	}

	// Later a failing dial may refetch once more.
	clock.Advance(rdsCertPoolMinRefetchInterval)
	if _, err := d.Dial("tcp", addr); err == nil {
		t.Fatal("Dial() to an untrusted server succeeded")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2", got)
	}
}

func TestRDSCertPoolCacheInvalidateOnlyCurrentPool(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, second := x509.NewCertPool(), x509.NewCertPool()
	f := &fakeCertFetcher{pools: []*x509.CertPool{first, second}}
	cache := newTestCertCache(f, clock)
	if _, err := cache.RDSCertPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(rdsCertPoolMinRefetchInterval)

	// Concurrent failures with the same pool cause one refetch.
	if !cache.invalidate(first) {
		t.Fatal("invalidate(current pool) = false")
	}
	if p, _ := cache.RDSCertPool(context.Background()); p != second {
		t.Fatal("pool not refetched after invalidate")
	}
	if !cache.invalidate(first) {
		t.Error("invalidate(replaced pool) = false, want true: the caller should retry with the new pool")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2", got)
	}
}
