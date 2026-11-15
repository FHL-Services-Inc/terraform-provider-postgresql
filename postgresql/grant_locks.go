package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// Object-keyed advisory locks.
//
// GRANT/REVOKE rewrite the ACL column of the catalog row of every object they
// touch (pg_class.relacl, pg_attribute.attacl, pg_namespace.nspacl,
// pg_proc.proacl, ...), and ALTER DEFAULT PRIVILEGES rewrites one
// pg_default_acl row per (owner, schema, object type). Catalog rows are updated
// in place: when two transactions change the same row, the second one waits
// for the first and then fails with "tuple concurrently updated" (XX000)
// instead of re-reading the row. The role lock is keyed by the grantee, so two
// grants on the same objects for different grantees (two roles, or a role and
// PUBLIC) are not serialized by it.
//
// The write paths therefore also lock what they change, keyed by the object
// (by name, within the current database: advisory locks are per database,
// like the catalogs above):
//
//   - a grant on all objects of a class in a schema (ALL TABLES IN SCHEMA s)
//     takes an exclusive lock on (class, schema);
//   - a grant on listed objects takes the same (class, schema) lock in shared
//     mode, then an exclusive lock per object. Grants on disjoint objects of
//     a schema still run in parallel, and either kind excludes a schema-wide
//     grant of the same class;
//   - schemas, foreign data wrappers and foreign servers: one exclusive lock
//     on the object; databases keep the database OID lock;
//   - default privileges: one exclusive lock on (owner, schema, object type),
//     i.e. the pg_default_acl row;
//   - REASSIGN OWNED (a postgresql_database owner change with
//     alter_object_ownership, a postgresql_role delete) changes the owner of
//     objects in the whole database: it takes one database-wide key
//     exclusively, and grant and default privileges writes take it shared,
//     before they read the owners of their objects. Without it a grant could
//     take the membership of the previous owner while REASSIGN OWNED gives
//     the objects to the new one, and fail with "permission denied" (#181).
//
// Lock order. Every transaction of the provider takes its advisory locks in
// one global order, so two of them can't wait for each other in a cycle:
//
//  1. the single-bigint OID keys (roles, members, the database of a database
//     grant), in one statement ordered by OID (roleLockQuery,
//     granteeLockQuery);
//  2. then the two-int4 object keys of this file: the database-wide REASSIGN
//     OWNED key, then schema-wide keys, then object keys sorted by key value
//     (advisoryLockStatement).
//
// No path takes an OID key after an object key, and nothing takes an advisory
// lock after withRolesGranted took its membership mutexes (membershipLocks).
//
// The object keys use the two-int4 form of pg_advisory_xact_lock. PostgreSQL
// keeps it in a separate key space from the single-bigint form used for the
// OID locks, so the two never collide. The first int is a namespace per kind
// of key; the second is a 32-bit FNV-1a hash of the parts. A hash collision
// only makes two unrelated writes wait for each other.
const (
	advisoryLockNamespaceReassignOwned int32 = 0x74667072 // "tfpr": REASSIGN OWNED in the current database
	advisoryLockNamespaceSchemaObjects int32 = 0x74667073 // "tfps": all objects of a class in a schema
	advisoryLockNamespaceObject        int32 = 0x7466706f // "tfpo": one object
	advisoryLockNamespaceDefaultACL    int32 = 0x74667064 // "tfpd": one pg_default_acl row
)

// advisoryLockKey is one two-int4 transaction-level advisory lock.
type advisoryLockKey struct {
	namespace int32
	key       int32
	shared    bool
}

func newAdvisoryLockKey(namespace int32, shared bool, parts ...string) advisoryLockKey {
	h := fnv.New32a()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return advisoryLockKey{namespace: namespace, key: int32(h.Sum32()), shared: shared} //nolint:gosec // reinterpreting the hash bits as int4 is intended
}

func (k advisoryLockKey) function() string {
	if k.shared {
		return "pg_advisory_xact_lock_shared"
	}
	return "pg_advisory_xact_lock"
}

// call returns the lock function call with its literal arguments.
func (k advisoryLockKey) call() string {
	return fmt.Sprintf("%s(%d, %d)", k.function(), k.namespace, k.key)
}

func (k advisoryLockKey) String() string {
	return k.call()
}

// namespaceOrder is the acquisition order of the namespaces: wider scopes first.
var namespaceOrder = map[int32]int{
	advisoryLockNamespaceReassignOwned: 0,
	advisoryLockNamespaceSchemaObjects: 1,
	advisoryLockNamespaceObject:        2,
	advisoryLockNamespaceDefaultACL:    3,
}

// reassignOwnedLockKey is the database-wide key of REASSIGN OWNED: exclusive
// for REASSIGN OWNED, shared for the writes that read object owners (#181).
func reassignOwnedLockKey(shared bool) advisoryLockKey {
	return newAdvisoryLockKey(advisoryLockNamespaceReassignOwned, shared, "reassign owned")
}

func sortAdvisoryLockKeys(keys []advisoryLockKey) {
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.namespace != b.namespace {
			return namespaceOrder[a.namespace] < namespaceOrder[b.namespace]
		}
		if a.key != b.key {
			return a.key < b.key
		}
		// Exclusive before shared for the same key. Callers never ask for both
		// on one key, but the order must be total to be deterministic.
		return !a.shared && b.shared
	})
}

// advisoryLockStatement returns the SQL taking keys in acquisition order (see
// sortAdvisoryLockKeys), duplicates removed, or "" for no keys. Keys of one
// namespace and mode go in a single statement; unnest ... WITH ORDINALITY ...
// ORDER BY keeps them in array order (the lock function is volatile, so
// PostgreSQL evaluates it after the sort). Several statements are joined into
// one string, sent in one round-trip. All values are integer literals.
func advisoryLockStatement(keys []advisoryLockKey) string {
	sorted := append([]advisoryLockKey(nil), keys...)
	sortAdvisoryLockKeys(sorted)

	var stmts []string
	for i := 0; i < len(sorted); {
		group := sorted[i]
		var values []string
		for ; i < len(sorted) && sorted[i].namespace == group.namespace && sorted[i].shared == group.shared; i++ {
			v := strconv.Itoa(int(sorted[i].key))
			if len(values) == 0 || values[len(values)-1] != v {
				values = append(values, v)
			}
		}
		if len(values) == 1 {
			stmts = append(stmts, "SELECT "+group.call())
			continue
		}
		stmts = append(stmts, fmt.Sprintf(
			"SELECT %s(%d, k) FROM unnest('{%s}'::int4[]) WITH ORDINALITY AS l(k, n) ORDER BY n",
			group.function(), group.namespace, strings.Join(values, ","),
		))
	}
	return strings.Join(stmts, ";\n")
}

// grantLockClass maps a grant object_type to the class of catalog rows it
// changes. Column grants change the table's pg_attribute rows, and a REVOKE on
// a table also revokes its column privileges, so they lock the table. ALL
// FUNCTIONS / ALL PROCEDURES / ALL ROUTINES all write pg_proc rows of the
// same schema, so they share a class.
func grantLockClass(objectType string) string {
	switch objectType {
	case "column":
		return "table"
	case "function", "procedure", "routine":
		return "routine"
	}
	return objectType
}

// grantLockKeys returns the object-keyed advisory locks a postgresql_grant
// write takes, in acquisition order: the shared REASSIGN OWNED key, then the
// object keys. Database grants return none: they run on the maintenance
// database and lock the database OID with the grantee (granteeLockQuery).
func grantLockKeys(get ResourceSchemeGetter) []advisoryLockKey {
	keys := grantObjectLockKeys(get)
	if len(keys) == 0 {
		return nil
	}
	return append([]advisoryLockKey{reassignOwnedLockKey(true)}, keys...)
}

// grantObjectLockKeys returns the object keys of grantLockKeys.
func grantObjectLockKeys(get ResourceSchemeGetter) []advisoryLockKey {
	objectType := get("object_type").(string)
	schemaName, _ := get("schema").(string)
	var objects []string
	if set, ok := get("objects").(*schema.Set); ok {
		for _, o := range set.List() {
			objects = append(objects, o.(string))
		}
	}

	switch objectType {
	case "database":
		return nil
	case "schema":
		return []advisoryLockKey{newAdvisoryLockKey(advisoryLockNamespaceObject, false, "schema", "", schemaName)}
	case "foreign_data_wrapper", "foreign_server":
		keys := make([]advisoryLockKey, 0, len(objects))
		for _, o := range objects {
			keys = append(keys, newAdvisoryLockKey(advisoryLockNamespaceObject, false, objectType, "", o))
		}
		sortAdvisoryLockKeys(keys)
		return keys
	}

	class := grantLockClass(objectType)
	if len(objects) == 0 {
		return []advisoryLockKey{newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, class, schemaName)}
	}

	keys := make([]advisoryLockKey, 0, len(objects))
	seen := map[advisoryLockKey]bool{}
	for _, o := range objects {
		if class == "routine" {
			// Lock routines by name: "f(integer)" and "f" may be the same
			// pg_proc row, and overloads sharing a key only costs a wait.
			if i := strings.IndexByte(o, '('); i >= 0 {
				o = strings.TrimSpace(o[:i])
			}
		}
		k := newAdvisoryLockKey(advisoryLockNamespaceObject, false, class, schemaName, o)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sortAdvisoryLockKeys(keys)
	return append([]advisoryLockKey{newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, true, class, schemaName)}, keys...)
}

// defaultPrivilegesLockKey returns the lock on the pg_default_acl row a
// postgresql_default_privileges write changes.
func defaultPrivilegesLockKey(d *schema.ResourceData) advisoryLockKey {
	return newAdvisoryLockKey(advisoryLockNamespaceDefaultACL, false,
		d.Get("owner").(string), d.Get("schema").(string), objectTypes[d.Get("object_type").(string)],
	)
}

// granteeLockQuery locks the OID of a role alone, not its members, and, when
// $2 is not empty, the OID of database $2, in one statement ordered by OID
// like roleLockQuery. Grant and default privileges writes change ACLs, not
// role memberships, so they don't need the member locks of pgLockRole.
//
// Taking a single role key (or the role and the database, ordered) can't
// reintroduce the lock-order deadlock of roleLockQuery (#111): that came from
// one transaction taking several OID keys out of order. Here the OID keys are
// taken in OID order, before any object key.
const granteeLockQuery = `SELECT pg_advisory_xact_lock(oid::bigint) FROM (
	SELECT oid FROM pg_roles WHERE rolname = $1
	UNION
	SELECT oid FROM pg_database WHERE $2::text <> '' AND datname = $2::text
) AS keys ORDER BY oid`

// pgLockGranteeAndObjects takes the advisory locks of a grant or default
// privileges write, in the global lock order (see above): the role (without
// its members) and, if database is not empty, the database, in one statement
// ordered by OID; then the object keys, in a separate statement.
func pgLockGranteeAndObjects(txn *sql.Tx, role, database string, keys []advisoryLockKey) error {
	// Disable statement timeout for this connection otherwise the lock could fail
	if _, err := txn.Exec("SET statement_timeout = 0"); err != nil {
		return fmt.Errorf("could not disable statement_timeout: %w", err)
	}
	if _, err := txn.Exec(granteeLockQuery, role, database); err != nil {
		if database != "" {
			return fmt.Errorf("could not get advisory locks for role %s and database %s: %w", role, database, err)
		}
		return fmt.Errorf("could not get advisory lock for role %s: %w", role, err)
	}
	if stmt := advisoryLockStatement(keys); stmt != "" {
		if _, err := txn.Exec(stmt); err != nil {
			return fmt.Errorf("could not get advisory locks for the objects role %s changes: %w", role, err)
		}
	}
	return nil
}

// isConcurrentCatalogUpdateError reports whether err is PostgreSQL's
// "tuple concurrently updated/deleted": a catalog row (an ACL, a membership)
// changed by another transaction that committed while this one waited for it.
func isConcurrentCatalogUpdateError(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code != "XX000" {
		return false
	}
	return strings.Contains(pqErr.Message, "tuple concurrently updated") ||
		strings.Contains(pqErr.Message, "tuple concurrently deleted")
}

// retryOnConcurrentCatalogUpdate wraps a Create/Update/Delete function so that
// it runs once more if it failed with "tuple concurrently updated". The
// object locks prevent this between writers of this provider in one database;
// the retry covers writers that don't take them (another tool, an older
// provider version, a manual GRANT) and the cluster-wide membership rows
// withRolesGranted changes from another provider process (#25). The failed
// attempt's transaction was rolled back (and its membership locks released)
// before fn returned, and the functions it wraps are idempotent (REVOKE then
// GRANT, then Read).
func retryOnConcurrentCatalogUpdate(fn func(*DBConnection, *schema.ResourceData) error) func(*DBConnection, *schema.ResourceData) error {
	return func(db *DBConnection, d *schema.ResourceData) error {
		err := fn(db, d)
		if isConcurrentCatalogUpdateError(err) {
			log.Printf("[WARN] %v; retrying once", err)
			err = fn(db, d)
		}
		return err
	}
}
