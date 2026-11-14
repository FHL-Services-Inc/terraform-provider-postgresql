package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// pubSubFakeDriver is a fake database/sql driver for the publication and
// subscription Read tests. It records transactions and queries and answers
// each query through respond (nil means "no rows"). When openErr is set,
// opening a connection fails with it (e.g. SQLSTATE 3D000).
type pubSubFakeDriver struct {
	mu      sync.Mutex
	begins  int
	queries []string
	openErr error
	respond func(query string) *pubSubFakeRows
}

func (d *pubSubFakeDriver) reset(openErr error, respond func(string) *pubSubFakeRows) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.begins = 0
	d.queries = nil
	d.openErr = openErr
	d.respond = respond
}

func (d *pubSubFakeDriver) counts() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, append([]string(nil), d.queries...)
}

func (d *pubSubFakeDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &pubSubFakeConn{driver: d}, nil
}

type pubSubFakeConn struct {
	driver *pubSubFakeDriver
}

func (c *pubSubFakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *pubSubFakeConn) Close() error                        { return nil }

func (c *pubSubFakeConn) Begin() (driver.Tx, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.begins++
	return pubSubFakeTx{}, nil
}

func (c *pubSubFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.queries = append(c.driver.queries, query)
	if c.driver.respond != nil {
		if rows := c.driver.respond(query); rows != nil {
			return rows, nil
		}
	}
	return &pubSubFakeRows{cols: []string{"c"}}, nil
}

type pubSubFakeTx struct{}

func (pubSubFakeTx) Commit() error   { return nil }
func (pubSubFakeTx) Rollback() error { return nil }

type pubSubFakeRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *pubSubFakeRows) Columns() []string { return r.cols }
func (r *pubSubFakeRows) Close() error      { return nil }

func (r *pubSubFakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

const (
	pubSubFakeDriverName = "pg-pubsub-read-fake"
	pubSubFakeDBName     = "pubsubdb"
)

var (
	pubSubFake         = &pubSubFakeDriver{}
	registerPubSubFake sync.Once
)

// newPubSubFakeConnection returns a DBConnection backed by pubSubFake and
// registers it as the connection for database pubSubFakeDBName, so the
// per-database lookup done by the Reads resolves to it instead of dialing.
func newPubSubFakeConnection(t *testing.T, openErr error, respond func(string) *pubSubFakeRows) *DBConnection {
	t.Helper()
	registerPubSubFake.Do(func() { sql.Register(pubSubFakeDriverName, pubSubFake) })
	pubSubFake.reset(openErr, respond)

	sqlDB, err := sql.Open(pubSubFakeDriverName, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	sqlDB.SetMaxIdleConns(0)

	// A non-"postgres" scheme keeps connStr down to a single query
	// parameter, so the DSN used as the registry key is stable.
	client := &Client{config: Config{Scheme: "awspostgres"}, databaseName: pubSubFakeDBName}
	conn := &DBConnection{DB: sqlDB, client: client, version: semver.MustParse("16.0.0")}

	dsn := client.config.connStr(client.databaseName)
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = sqlDB.Close()
	})

	return conn
}

// pubSubFound answers the publication/subscription queries as if both
// objects existed.
func pubSubFound(query string) *pubSubFakeRows {
	switch {
	case strings.Contains(query, "FROM pg_catalog.pg_publication as p"):
		// puballtables, pubinsert, pubupdate, pubdelete, pubownername, pubviaroot, pubtruncate
		return &pubSubFakeRows{
			cols: []string{"puballtables", "pubinsert", "pubupdate", "pubdelete", "pubownername", "pubviaroot", "pubtruncate"},
			rows: [][]driver.Value{{false, true, true, true, "owner", false, true}},
		}
	case strings.Contains(query, "FROM pg_catalog.pg_publication_tables"):
		return &pubSubFakeRows{
			cols: []string{"fulltablename"},
			rows: [][]driver.Value{{"public.t1"}, {"public.t2"}},
		}
	case strings.Contains(query, "FROM pg_catalog.pg_stat_subscription"):
		return &pubSubFakeRows{cols: []string{"bool"}, rows: [][]driver.Value{{true}}}
	case strings.Contains(query, "FROM pg_catalog.pg_subscription"):
		return &pubSubFakeRows{
			cols: []string{"subconninfo", "subpublications", "subslotname"},
			rows: [][]driver.Value{{"host=src dbname=src", []byte("{pub}"), "slot"}},
		}
	}
	return nil
}

type pubSubReadCase struct {
	name  string
	res   *schema.Resource
	read  func(*DBConnection, *schema.ResourceData) error
	id    string
	attrs map[string]any
}

func pubSubReadCases() []pubSubReadCase {
	return []pubSubReadCase{
		{
			name:  "publication",
			res:   resourcePostgreSQLPublication(),
			read:  resourcePostgreSQLPublicationRead,
			id:    pubSubFakeDBName + ".pub",
			attrs: map[string]any{pubNameAttr: "pub", pubDatabaseAttr: pubSubFakeDBName},
		},
		{
			name: "subscription",
			res:  resourcePostgreSQLSubscription(),
			read: resourcePostgreSQLSubscriptionRead,
			id:   pubSubFakeDBName + ".sub",
			attrs: map[string]any{
				"name":         "sub",
				"database":     pubSubFakeDBName,
				"conninfo":     "host=src dbname=src",
				"publications": []any{"pub"},
			},
		},
	}
}

// TestPubSubReadWithoutTransaction pins that the publication and subscription
// Reads run their two SELECTs directly instead of inside BEGIN ... ROLLBACK,
// which only cost two extra round-trips per resource on every refresh.
func TestPubSubReadWithoutTransaction(t *testing.T) {
	for _, tc := range pubSubReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := newPubSubFakeConnection(t, nil, pubSubFound)

			d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			d.SetId(tc.id)

			if err := tc.read(conn, d); err != nil {
				t.Fatalf("Read returned error: %v", err)
			}

			begins, queries := pubSubFake.counts()
			if begins != 0 {
				t.Errorf("Read opened %d transaction(s), want 0", begins)
			}
			if len(queries) != 2 {
				t.Errorf("Read issued %d queries, want 2: %q", len(queries), queries)
			}
			if d.Id() != tc.id {
				t.Errorf("Read changed ID to %q, want %q", d.Id(), tc.id)
			}
		})
	}

	// Values read from the fake rows end up in state.
	t.Run("publication values", func(t *testing.T) {
		conn := newPubSubFakeConnection(t, nil, pubSubFound)
		tc := pubSubReadCases()[0]
		d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
		d.SetId(tc.id)
		if err := tc.read(conn, d); err != nil {
			t.Fatalf("Read returned error: %v", err)
		}
		if got := d.Get(pubOwnerAttr).(string); got != "owner" {
			t.Errorf("owner = %q, want %q", got, "owner")
		}
		if got := d.Get(pubTablesAttr).(*schema.Set).Len(); got != 2 {
			t.Errorf("tables has %d elements, want 2", got)
		}
	})
}

// TestPubSubReadNotFound pins that a missing publication/subscription is still
// removed from state after a single query.
func TestPubSubReadNotFound(t *testing.T) {
	for _, tc := range pubSubReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := newPubSubFakeConnection(t, nil, nil)

			d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			d.SetId(tc.id)

			if err := tc.read(conn, d); err != nil {
				t.Fatalf("Read returned error: %v", err)
			}

			begins, queries := pubSubFake.counts()
			if begins != 0 {
				t.Errorf("Read opened %d transaction(s), want 0", begins)
			}
			if len(queries) != 1 {
				t.Errorf("Read issued %d queries, want 1: %q", len(queries), queries)
			}
			if d.Id() != "" {
				t.Errorf("Read of a missing object kept ID %q, want it cleared", d.Id())
			}
		})
	}
}

// TestPubSubReadPrunesWhenDatabaseDropped pins that the Reads still remove the
// resource from state when connecting to its database fails with SQLSTATE
// 3D000 (database dropped out-of-band), now that the error surfaces on the
// first query instead of on BEGIN.
func TestPubSubReadPrunesWhenDatabaseDropped(t *testing.T) {
	dropped := &pq.Error{Code: "3D000", Message: `database "pubsubdb" does not exist`}

	for _, tc := range pubSubReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := newPubSubFakeConnection(t, dropped, nil)

			d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			d.SetId(tc.id)

			if err := tc.read(conn, d); err != nil {
				t.Fatalf("Read returned error, want the resource pruned: %v", err)
			}
			if d.Id() != "" {
				t.Errorf("Read kept ID %q for a dropped database, want it cleared", d.Id())
			}
		})
	}
}
