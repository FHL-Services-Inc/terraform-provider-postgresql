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
// instead of re-reading the row. The role lock taken by the write paths is
// keyed by the grantee, so two grants on the same objects for different
// grantees (two roles, or a role and PUBLIC) are not serialized by it.
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
//     on the object; databases keep pgLockDatabase;
//   - default privileges: one exclusive lock on (owner, schema, object type),
//     i.e. the pg_default_acl row.
//
// Locks are taken after the role lock, schema-wide keys first, then object keys
// sorted by key, so every transaction acquires them in the same global order
// and two writers can't deadlock on them.
//
// The keys use the two-int4 form of pg_advisory_xact_lock. PostgreSQL keeps it
// in a separate key space from the single-bigint form used for role and
// database OID locks, so the two never collide. The first int is a namespace
// per kind of key; the second is a 32-bit FNV-1a hash of the parts. A hash
// collision only makes two unrelated writes wait for each other.
const (
	advisoryLockNamespaceSchemaObjects int32 = 0x74667073 // "tfps": all objects of a class in a schema
	advisoryLockNamespaceObject        int32 = 0x7466706f // "tfpo": one object
	advisoryLockNamespaceDefaultACL    int32 = 0x74667064 // "tfpd": one pg_default_acl row
	advisoryLockNamespaceMembership    int32 = 0x7466706d // "tfpm": one pg_auth_members row
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
	advisoryLockNamespaceSchemaObjects: 0,
	advisoryLockNamespaceObject:        1,
	advisoryLockNamespaceDefaultACL:    2,
	advisoryLockNamespaceMembership:    3,
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

// advisoryLockStatements returns the statements taking keys in acquisition
// order (see sortAdvisoryLockKeys), duplicates removed. Keys of one namespace
// and mode go in a single statement; unnest ... WITH ORDINALITY ... ORDER BY
// keeps them in array order (the lock function is volatile, so PostgreSQL
// evaluates it after the sort).
func advisoryLockStatements(keys []advisoryLockKey) []string {
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
	return stmts
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
// write takes, in acquisition order. Database grants return none: they keep
// the database OID lock (pgLockDatabase).
func grantLockKeys(get ResourceSchemeGetter) []advisoryLockKey {
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

// membershipLockKey returns the lock on the pg_auth_members row granting role
// to member.
func membershipLockKey(role, member string) advisoryLockKey {
	return newAdvisoryLockKey(advisoryLockNamespaceMembership, false, role, member)
}

// roleLockStatement locks the role OID only, not its members. Locking the
// members is for paths that change memberships of the role (pgLockRole).
func roleLockStatement(role string) string {
	return "SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = " + pq.QuoteLiteral(role)
}

func databaseLockStatement(database string) string {
	return "SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_database WHERE datname = " + pq.QuoteLiteral(database)
}

// pgLockRoleAndObjects locks role (its OID only, not its members) and then the
// given object keys, in one round-trip. Used by the grant and default
// privileges paths, which change ACLs but no role membership.
func pgLockRoleAndObjects(db *DBConnection, txn *sql.Tx, role string, extra []string, keys []advisoryLockKey) error {
	stmts := append([]string{roleLockStatement(role)}, extra...)
	stmts = append(stmts, advisoryLockStatements(keys)...)
	return execAdvisoryLock(db, txn,
		fmt.Sprintf("role %q or the objects it changes", role),
		fmt.Sprintf("could not get advisory lock for role %s or the objects it changes", role),
		stmts...,
	)
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
// object locks prevent this between writers of this provider; the retry
// covers writers that don't take them (another tool, an older provider
// version, a manual GRANT). The failed attempt's transaction was rolled back,
// and the functions it wraps are idempotent (REVOKE then GRANT, then Read).
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
