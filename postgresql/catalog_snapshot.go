package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"golang.org/x/sync/singleflight"
)

const defaultCatalogSnapshotTTLSeconds = 30

// catalogSnapshotMaxBytes bounds the catalog text (names and ACLs) one
// snapshot may hold. A database above it is read directly instead.
var catalogSnapshotMaxBytes = 64 << 20

// catalogSnapshotQuery reads every catalog the grant and default privileges
// Reads consult, in one statement so the whole snapshot is one MVCC snapshot.
// Rows are tagged by their first column:
//
//	r: roles            (oid, 0, rolname, NULL, NULL)
//	n: namespaces       (oid, 0, nspname, NULL, nspacl)
//	c: relations        (0, relnamespace, relname, relkind, relacl) for the
//	   relkinds the grant Read handles (ownerRelkinds: tables, partitioned
//	   tables, views, materialized views, foreign tables and sequences)
//	d: default ACLs     (defaclrole, defaclnamespace, NULL, defaclobjtype, defaclacl)
//
// The last column tells whether a relation is an extension member, which a
// grant on all objects of the schema leaves out (see notExtensionMember); it
// is false for the other rows.
//
// pg_database is not in it: database grants are read on the maintenance
// database (grantConnDatabase), not on the database the snapshot is taken on.
//
// A NULL object ACL is read as acldefault(type, owner), exactly as the direct
// Read queries do (see relationACLType), so an object never granted or revoked
// on explicitly reads with its owner's (and PUBLIC's) default privileges.
// Default ACLs are read as they are, as the direct default privileges Read
// does.
//
// ACLs come back in their text form (aclitem[]::text) and are parsed in Go
// when a Read needs them: exploding them on the server costs several times the
// raw read on large catalogs.
const catalogSnapshotQuery = `SELECT 'r', oid::bigint, 0::bigint, rolname::text, NULL::text, NULL::text, false FROM pg_catalog.pg_roles
UNION ALL SELECT 'n', oid::bigint, 0::bigint, nspname::text, NULL, COALESCE(nspacl, pg_catalog.acldefault('n', nspowner))::text, false FROM pg_catalog.pg_namespace
UNION ALL SELECT 'c', 0::bigint, relnamespace::bigint, relname::text, relkind::text, COALESCE(relacl, pg_catalog.acldefault(CASE WHEN relkind = 'S' THEN 's' ELSE 'r' END::"char", relowner))::text, EXISTS (SELECT 1 FROM pg_catalog.pg_depend dep WHERE dep.classid = 'pg_catalog.pg_class'::regclass AND dep.objid = pg_class.oid AND dep.deptype = 'e') FROM pg_catalog.pg_class WHERE relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
UNION ALL SELECT 'd', defaclrole::bigint, defaclnamespace::bigint, NULL, defaclobjtype::text, defaclacl::text, false FROM pg_catalog.pg_default_acl
`

// catalogSnapshotProcQuery loads pg_proc on demand: only Reads of function,
// procedure and routine grants need it, and PostGIS-like extensions make it
// the largest ACL-bearing catalog after pg_class. A NULL proacl reads as
// acldefault('f', owner), as in the direct Read. Servers without
// pg_proc.prokind (before PostgreSQL 11, no procedures) use
// catalogSnapshotProcQueryNoProkind, where every routine is a function.
const catalogSnapshotProcQuery = `SELECT pronamespace::bigint, proname::text, prokind::text, COALESCE(proacl, pg_catalog.acldefault('f', proowner))::text, ` + catalogSnapshotProcExtensionMember + ` FROM pg_catalog.pg_proc
`

const catalogSnapshotProcQueryNoProkind = `SELECT pronamespace::bigint, proname::text, 'f'::text, COALESCE(proacl, pg_catalog.acldefault('f', proowner))::text, ` + catalogSnapshotProcExtensionMember + ` FROM pg_catalog.pg_proc
`

// catalogSnapshotProcExtensionMember tells whether a routine is an extension
// member (see notExtensionMember).
const catalogSnapshotProcExtensionMember = `EXISTS (SELECT 1 FROM pg_catalog.pg_depend dep WHERE dep.classid = 'pg_catalog.pg_proc'::regclass AND dep.objid = pg_proc.oid AND dep.deptype = 'e')`

type catalogObject struct {
	name string
	// kind is relkind for relations, prokind for routines.
	kind string
	acl  sql.NullString
	// extensionMember is true for an object that belongs to an extension.
	extensionMember bool
}

type catalogNamespace struct {
	oid uint32
	acl sql.NullString
}

type defaultACLKey struct {
	owner     uint32
	namespace uint32
	objType   string
}

// catalogSnapshot is the ACL-relevant part of one database's catalogs, as
// read by catalogSnapshotQuery, shared by every grant and default privileges
// Read on that database until a write by this process or the TTL drops it.
type catalogSnapshot struct {
	// gen is the generation of the cache entry the snapshot was built for; a
	// snapshot whose generation no longer matches was built across a write.
	gen     uint64
	builtAt time.Time
	// db is the pool of the snapshot's database, used to load pg_proc.
	db *DBConnection

	roles      map[string]uint32
	namespaces map[string]catalogNamespace
	// relations holds the relations of each namespace that a table or
	// sequence grant covers (ownerRelkinds), by name.
	relations  map[uint32][]catalogObject
	defaultACL map[defaultACLKey]string

	// routines is loaded by the first Read that needs pg_proc. Overloads are
	// adjacent entries with the same name.
	procMu   sync.Mutex
	routines map[uint32][]catalogObject
}

// catalogSnapshotEntry is the cache slot of one DSN.
type catalogSnapshotEntry struct {
	// gen is bumped by every invalidation, so a build that straddles a write
	// is recognised and not published.
	gen  uint64
	snap *catalogSnapshot
	// skipUntil makes Reads go direct for a while after a build failed or
	// found the database too large, instead of retrying on every Read.
	skipUntil time.Time
}

type catalogSnapshotCache struct {
	mu      sync.Mutex
	entries map[string]*catalogSnapshotEntry
	group   singleflight.Group
	// builds counts the snapshots built, for tests and logs.
	builds int
}

var catalogSnapshots = &catalogSnapshotCache{entries: map[string]*catalogSnapshotEntry{}}

var errCatalogSnapshotTooLarge = errors.New("catalog snapshot exceeds the memory cap")

func (c *catalogSnapshotCache) entry(dsn string) *catalogSnapshotEntry {
	e, ok := c.entries[dsn]
	if !ok {
		e = &catalogSnapshotEntry{}
		c.entries[dsn] = e
	}
	return e
}

// invalidate drops the snapshot of dsn and marks any build in flight stale.
func (c *catalogSnapshotCache) invalidate(dsn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(dsn)
	e.gen++
	e.snap = nil
}

// invalidateAll drops every snapshot: for writes to cluster-wide catalogs.
func (c *catalogSnapshotCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.gen++
		e.snap = nil
	}
}

// get returns the current snapshot of db's database, building it when there
// is none (concurrent callers share one build). It returns nil when the Read
// should query the database directly: a recent build failed or the database is
// above the memory cap. A build error for a database that no longer exists
// (SQLSTATE 3D000) is returned, so the caller prunes the resource as the
// direct Read would.
func (c *catalogSnapshotCache) get(db *DBConnection) (*catalogSnapshot, error) {
	dsn := db.client.config.connStr(db.client.databaseName)
	ttl := time.Duration(db.client.config.CatalogSnapshotTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultCatalogSnapshotTTLSeconds * time.Second
	}

	// A snapshot is fresh when it is younger than the TTL and no write landed
	// since the build started.
	lookup := func() (*catalogSnapshot, uint64, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		e := c.entry(dsn)
		if e.snap != nil && e.snap.gen == e.gen && time.Since(e.snap.builtAt) < ttl {
			return e.snap, e.gen, false
		}
		return nil, e.gen, time.Now().Before(e.skipUntil)
	}

	// At most two builds: the second only when a write landed during the
	// first. A database written continuously is read directly rather than
	// rebuilt forever.
	for attempt := 0; attempt < 2; attempt++ {
		snap, gen, skip := lookup()
		if snap != nil {
			return snap, nil
		}
		if skip {
			return nil, nil
		}

		v, err, _ := c.group.Do(dsn, func() (any, error) {
			if snap, _, _ := lookup(); snap != nil {
				// Published by a flight that finished between the lookup and Do.
				return snap, nil
			}
			snap, err := buildCatalogSnapshot(db, gen)
			c.mu.Lock()
			defer c.mu.Unlock()
			e := c.entry(dsn)
			if err != nil {
				if !isDatabaseDoesNotExistError(err) {
					e.skipUntil = time.Now().Add(ttl)
				}
				return nil, err
			}
			c.builds++
			if e.gen == snap.gen {
				e.snap = snap
			}
			return snap, nil
		})
		if err != nil {
			if isDatabaseDoesNotExistError(err) {
				return nil, err
			}
			log.Printf("[WARN] catalog snapshot of database %s not available, reading directly: %v", db.client.databaseName, err)
			return nil, nil
		}
		snap = v.(*catalogSnapshot)
		if _, gen, _ := lookup(); snap.gen == gen {
			return snap, nil
		}
		log.Printf("[DEBUG] catalog snapshot of database %s was built across a write, rebuilding", db.client.databaseName)
	}
	return nil, nil
}

// buildCatalogSnapshot runs catalogSnapshotQuery on db and indexes the result.
func buildCatalogSnapshot(db *DBConnection, gen uint64) (*catalogSnapshot, error) {
	start := time.Now()
	rows, err := db.Query(catalogSnapshotQuery)
	if err != nil {
		return nil, err
	}
	defer closeGrantReadRows(rows)

	snap := &catalogSnapshot{
		gen:        gen,
		builtAt:    start,
		db:         db,
		roles:      map[string]uint32{},
		namespaces: map[string]catalogNamespace{},
		relations:  map[uint32][]catalogObject{},
		defaultACL: map[defaultACLKey]string{},
	}
	var size, count int
	for rows.Next() {
		var kind string
		var id, namespace int64
		var name, sub, acl sql.NullString
		var extensionMember bool
		if err := rows.Scan(&kind, &id, &namespace, &name, &sub, &acl, &extensionMember); err != nil {
			return nil, err
		}
		if size += len(name.String) + len(acl.String); size > catalogSnapshotMaxBytes {
			return nil, errCatalogSnapshotTooLarge
		}
		count++

		switch kind {
		case "r":
			snap.roles[name.String] = uint32(id)
		case "n":
			snap.namespaces[name.String] = catalogNamespace{oid: uint32(id), acl: acl}
		case "c":
			snap.relations[uint32(namespace)] = append(snap.relations[uint32(namespace)], catalogObject{name: name.String, kind: sub.String, acl: acl, extensionMember: extensionMember})
		case "d":
			snap.defaultACL[defaultACLKey{owner: uint32(id), namespace: uint32(namespace), objType: sub.String}] = acl.String
		default:
			return nil, fmt.Errorf("catalog snapshot: unexpected row kind %q", kind)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, objects := range snap.relations {
		sortCatalogObjects(objects)
	}
	log.Printf("[DEBUG] catalog snapshot of database %s: %d rows, %d bytes of catalog text, %s", db.client.databaseName, count, size, time.Since(start))
	return snap, nil
}

func sortCatalogObjects(objects []catalogObject) {
	sort.SliceStable(objects, func(i, j int) bool { return objects[i].name < objects[j].name })
}

// routinesOf returns the routines of a namespace, loading pg_proc into the
// snapshot on first use.
func (s *catalogSnapshot) routinesOf(namespace uint32) ([]catalogObject, error) {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.routines == nil {
		routines, err := loadCatalogRoutines(s.db)
		if err != nil {
			return nil, err
		}
		s.routines = routines
	}
	return s.routines[namespace], nil
}

func loadCatalogRoutines(db *DBConnection) (map[uint32][]catalogObject, error) {
	query := catalogSnapshotProcQuery
	if !db.featureSupported(featureProcedure) {
		query = catalogSnapshotProcQueryNoProkind
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer closeGrantReadRows(rows)

	routines := map[uint32][]catalogObject{}
	var size int
	for rows.Next() {
		var namespace int64
		var name, kind string
		var acl sql.NullString
		var extensionMember bool
		if err := rows.Scan(&namespace, &name, &kind, &acl, &extensionMember); err != nil {
			return nil, err
		}
		if size += len(name) + len(acl.String); size > catalogSnapshotMaxBytes {
			return nil, errCatalogSnapshotTooLarge
		}
		routines[uint32(namespace)] = append(routines[uint32(namespace)], catalogObject{name: name, kind: kind, acl: acl, extensionMember: extensionMember})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, objects := range routines {
		sortCatalogObjects(objects)
	}
	return routines, nil
}

// granteeOf returns the grantee the Reads look for in an ACL: PUBLIC has no
// pg_roles row and is matched as the empty grantee; any other role must exist.
func (s *catalogSnapshot) granteeOf(role string) (grantee string, exists bool) {
	if role == publicRole {
		return "", true
	}
	_, exists = s.roles[role]
	return role, exists
}

// privilegesOf returns the privileges the ACL grants to grantee as a set, the
// way the direct Read aggregates aclexplode rows: every grantor, with or
// without grant option. The snapshot queries never return a NULL object ACL
// (it is read as acldefault); a NULL here grants nothing.
func privilegesOf(acl sql.NullString, grantee string) (*schema.Set, error) {
	var privileges []string
	if acl.Valid {
		var err error
		if privileges, err = aclPrivilegesGrantedTo(acl.String, grantee); err != nil {
			return nil, err
		}
	}
	return stringSliceToSet(privileges), nil
}

// invalidateCatalogSnapshot drops the snapshot of database (client's database
// when empty) after a write to it.
func invalidateCatalogSnapshot(client *Client, database string) {
	if database == "" {
		database = client.databaseName
	}
	catalogSnapshots.invalidate(client.config.connStr(database))
}

// invalidateCatalogSnapshots drops every snapshot, after a write to a
// cluster-wide catalog (roles, memberships, databases).
func invalidateCatalogSnapshots() {
	catalogSnapshots.invalidateAll()
}

// grantReadSnapshot returns the catalog snapshot a grant or default privileges
// Read of d may use, or nil when the Read must query the database directly
// (snapshots disabled, build failed, database too large or unreachable). When
// the database no longer exists (SQLSTATE 3D000) the resource is removed from
// state, as queryGrantRead does, and gone is true.
func grantReadSnapshot(db *DBConnection, d *schema.ResourceData) (snap *catalogSnapshot, gone bool) {
	if !db.client.config.CatalogSnapshot {
		return nil, false
	}
	database := d.Get("database").(string)
	target, err := initConnection(db.client, database)
	if err == nil {
		snap, err = catalogSnapshots.get(target)
	}
	if isDatabaseDoesNotExistError(err) {
		log.Printf("[WARN] PostgreSQL database (%s) not found, removing %s from state", database, d.Id())
		d.SetId("")
		return nil, true
	}
	if err != nil {
		log.Printf("[WARN] catalog snapshot of database %s not available, reading directly: %v", database, err)
		return nil, false
	}
	return snap, false
}
