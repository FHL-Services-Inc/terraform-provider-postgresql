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
// before withRolesGranted (pgLockRole, pgLockDatabase) only exist in the
// database of the resource. Two resources in different databases with the
// same owner therefore both revoke, grant and restore the same membership rows
// at the same time; when one of them revokes a row the other one deleted and
// re-created in a transaction that is not committed yet, PostgreSQL (up to 15)
// fails with "tuple concurrently deleted" (#25).
//
// Invariants:
//   - advisory locks are taken before these mutexes, never while holding one:
//     every caller takes its pgLockRole/pgLockDatabase before withRolesGranted,
//     and nothing between withRolesGranted and the end of its transaction takes
//     an advisory lock or calls withRolesGranted again;
//   - the mutexes are held from before the first membership change until the
//     transaction that made the changes commits or rolls back (commitTxn,
//     deferredRollback), so anything waiting on those rows in PostgreSQL is a
//     transaction that already holds the mutexes, never one waiting for them.
//
// PostgreSQL already serializes these transactions on the unique index of
// pg_auth_members (the second GRANT of the same membership waits for the first
// transaction to end), so holding the mutexes costs no parallelism.
var membershipLocks = newKeyedMutex()

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
