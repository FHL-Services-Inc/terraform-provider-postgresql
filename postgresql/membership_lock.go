package postgresql

import (
	"database/sql"
	"slices"
	"sync"
)

// keyedMutex is a set of mutexes keyed by name. lock takes the mutexes of
// several keys at once, always in sorted order, so two callers asking for
// overlapping sets in a different order cannot deadlock each other.
//
// Entries are never removed: keys are role names, and their number is bounded
// by the roles a configuration manages.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// lock takes the mutexes of keys (duplicates are ignored) in sorted order and
// returns the function that releases them. The mutexes are not reentrant: a
// goroutine must release before it locks any of the same keys again.
func (k *keyedMutex) lock(keys ...string) (unlock func()) {
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)

	mutexes := make([]*sync.Mutex, len(keys))
	k.mu.Lock()
	for i, key := range keys {
		m, ok := k.locks[key]
		if !ok {
			m = &sync.Mutex{}
			k.locks[key] = m
		}
		mutexes[i] = m
	}
	k.mu.Unlock()

	for _, m := range mutexes {
		m.Lock()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(mutexes) - 1; i >= 0; i-- {
				mutexes[i].Unlock()
			}
		})
	}
}

// membershipLocks serializes, within the provider process, the temporary
// membership changes withRolesGranted makes, per role name.
//
// pg_auth_members is a cluster-wide catalog, while the advisory locks taken
// before withRolesGranted (pgLockRole, pgLockGranteeAndObjects) only exist in
// the database of the resource. Two resources in different databases with the
// same owner therefore both revoke, grant and restore the same membership rows
// at the same time; when one of them revokes a row the other one deleted and
// re-created in a transaction that is not committed yet, PostgreSQL (up to 15)
// fails with "tuple concurrently deleted" (#25).
//
// Invariants:
//   - advisory locks are taken before these mutexes, never while holding one:
//     every caller takes its pgLockRole/pgLockGranteeAndObjects before
//     withRolesGranted, and nothing between withRolesGranted and the end of
//     its transaction takes an advisory lock or calls withRolesGranted again;
//   - the mutexes are held from before the first membership change until the
//     transaction that made the changes commits or rolls back (commitTxn,
//     deferredRollback), so anything waiting on those rows in PostgreSQL is a
//     transaction that already holds the mutexes, never one waiting for them.
//
// PostgreSQL already serializes these transactions on the unique index of
// pg_auth_members (the second GRANT of the same membership waits for the first
// transaction to end), so holding the mutexes costs no parallelism.
//
// The postgresql_database operations (createDatabase, resourcePostgreSQLDatabaseDelete,
// setDBOwner, setAlterOwnership) take the same mutexes, for the roles they
// grant to the provider user in autocommit, from before the GRANT until after
// the REVOKE (lockDatabaseMemberships). Without that, withRolesGranted could
// see such a membership, skip its own GRANT and run its statements after the
// REVOKE, where GRANT/REVOKE on an object only emit a WARNING ("no privileges
// were granted"); or grant it itself and then revoke the other one's
// membership (#152). Their rules:
//   - the role lock (lockRoleConn) and every connection they need are taken
//     before the mutexes; nothing waits for a pool connection while holding
//     one, so a transaction that waits for the mutex while holding the last
//     connection of its pool cannot block the holder;
//   - a transaction waiting for the mutexes holds nothing the holder can wait
//     for in PostgreSQL other than advisory locks, which the database
//     operations do not take under the mutex. Callers of withRolesGranted that
//     run DDL before it (resourcePostgreSQLSchemaUpdate) take the mutexes
//     first with lockMembershipsForTxn.
//
// The paths added since keep these rules:
//   - withRolesGrantedOrDirect takes the mutexes before it reads the
//     membership states, so a membership another operation granted in
//     autocommit is never seen as "already a member". Everything it runs
//     under them (the savepointed GRANT, the circular-chain lookup, the
//     directGrant check of postgresql_grant) uses the caller's transaction:
//     no new connection, no advisory lock;
//   - postgresql_role Delete runs REASSIGN/DROP OWNED in one transaction per
//     database, each one opened on its pool and holding pgLockRole before
//     withRolesGranted, and committed before the next database starts; the
//     transaction in the provider's database is only opened after them;
//   - with max_total_connections the connections of every pool share one
//     budget, so "no pool connection is awaited under a mutex" now means no
//     new connection at all. None of the paths above dials under a mutex.
//
// Limit: the mutexes are keyed by the roles the caller names. A membership
// the provider user inherits through another role (pg_has_role) is not
// serialized when that other role is the one a postgresql_database operation
// grants.
var membershipLocks = newKeyedMutex()

// lockDatabaseMemberships takes the membership locks of roles for a
// postgresql_database operation that grants them to the provider user outside
// of a transaction, and returns the function that releases them. The caller
// must already hold every connection it needs (see membershipLocks) and
// release the locks only after it has revoked the memberships again.
func lockDatabaseMemberships(roles ...string) (unlock func()) {
	return membershipLocks.lock(slices.DeleteFunc(slices.Clone(roles), func(r string) bool { return r == "" })...)
}

// txnMembershipUnlocks holds the release function of the membership locks
// taken on a transaction (*sql.Tx), until the transaction ends.
var txnMembershipUnlocks sync.Map

// lockMembershipsForTxn takes the membership locks of roles and keeps them
// until txn is committed with commitTxn or rolled back with deferredRollback.
// A transaction takes them at most once; a second call on the same
// transaction is a no-op if it asks for no new role, and otherwise returns
// false, because taking more keys later would break the lock order.
func lockMembershipsForTxn(txn *sql.Tx, roles []string) bool {
	if held, ok := txnMembershipUnlocks.Load(txn); ok {
		for _, role := range roles {
			if !slices.Contains(held.(*txnMembershipLock).roles, role) {
				return false
			}
		}
		return true
	}
	unlock := membershipLocks.lock(roles...)
	txnMembershipUnlocks.Store(txn, &txnMembershipLock{roles: slices.Clone(roles), unlock: unlock})
	return true
}

type txnMembershipLock struct {
	roles  []string
	unlock func()
}

// releaseMembershipLocks releases the membership locks held by txn, if any.
func releaseMembershipLocks(txn *sql.Tx) {
	if held, ok := txnMembershipUnlocks.LoadAndDelete(txn); ok {
		held.(*txnMembershipLock).unlock()
	}
}

// commitTxn commits txn and then releases the membership locks
// withRolesGranted took on it. Transactions that went through
// withRolesGranted must be committed with it, not with txn.Commit, so the
// locks are not held while the caller goes on (e.g. into a Read that takes
// advisory locks again).
func commitTxn(txn *sql.Tx) error {
	defer releaseMembershipLocks(txn)
	return txn.Commit()
}
