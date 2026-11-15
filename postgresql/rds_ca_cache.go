package postgresql

import (
	"context"
	"crypto/x509"
	"log"
	"time"

	"gocloud.dev/aws/rds"
)

const (
	// rdsCertPoolTTL is how long a downloaded RDS CA bundle is used before it
	// is downloaded again. AWS adds CAs to the bundle long before servers
	// use them, so a day is plenty fresh; a server presenting a CA the cached
	// bundle doesn't know triggers a refetch anyway (see rdsTLSDialer).
	rdsCertPoolTTL = 24 * time.Hour

	// rdsCertPoolMinRefetchInterval is the minimum age of a bundle before a
	// failed verification may replace it, so a server with a certificate no
	// RDS CA signed doesn't cause a download per dial.
	rdsCertPoolMinRefetchInterval = time.Minute
)

// defaultRDSCertPool is the RDS CA bundle cache shared by all awspostgres
// pools of the provider process.
var defaultRDSCertPool = newRDSCertPoolCache(new(rds.CertFetcher))

// rdsCertPoolCache is an rds.CertPoolProvider that keeps the parsed RDS CA
// bundle for rdsCertPoolTTL instead of downloading it on every dial, as
// gocloud's awspostgres does.
//
// It fails closed: without a bundle, RDSCertPool returns an error and no
// connection is made. If a refresh fails while an older bundle is cached,
// the older bundle keeps being used: servers are still verified against it.
// Only one caller downloads at a time; the others wait for its result.
type rdsCertPoolCache struct {
	fetch rds.CertPoolProvider
	now   func() time.Time

	// sem is a one-slot semaphore guarding the fields below (a channel so
	// that waiters give up when their context is done).
	sem       chan struct{}
	pool      *x509.CertPool
	fetchedAt time.Time
	stale     bool
}

func newRDSCertPoolCache(fetch rds.CertPoolProvider) *rdsCertPoolCache {
	return &rdsCertPoolCache{
		fetch: fetch,
		now:   time.Now,
		sem:   make(chan struct{}, 1),
	}
}

func (c *rdsCertPoolCache) lock(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *rdsCertPoolCache) unlock() { <-c.sem }

// RDSCertPool implements rds.CertPoolProvider.
func (c *rdsCertPoolCache) RDSCertPool(ctx context.Context) (*x509.CertPool, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer c.unlock()

	now := c.now()
	if c.pool != nil && !c.stale && now.Sub(c.fetchedAt) < rdsCertPoolTTL {
		return c.pool, nil
	}

	pool, err := c.fetch.RDSCertPool(ctx)
	if err != nil {
		if c.pool != nil {
			log.Printf("[WARN] could not refresh the RDS CA bundle, using the one from %s: %v",
				c.fetchedAt.Format(time.RFC3339), err)
			return c.pool, nil
		}
		return nil, err
	}
	c.pool, c.fetchedAt, c.stale = pool, now, false
	return pool, nil
}

// invalidate is called when a server's certificate didn't verify against
// pool because its CA is unknown. It reports whether the caller should
// retry with a fresh pool: true if pool was already replaced, or if it is
// old enough to be refetched (it is then marked stale); false if pool was
// fetched less than rdsCertPoolMinRefetchInterval ago.
func (c *rdsCertPoolCache) invalidate(pool *x509.CertPool) bool {
	c.sem <- struct{}{}
	defer c.unlock()

	if c.pool != pool {
		return true
	}
	if c.now().Sub(c.fetchedAt) < rdsCertPoolMinRefetchInterval {
		return false
	}
	c.stale = true
	return true
}
