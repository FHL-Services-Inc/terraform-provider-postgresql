package postgresql

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitOrFail fails the test if done is not closed within a second.
func waitOrFail(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out: %s", what)
	}
}

func TestKeyedMutexExcludesSameKey(t *testing.T) {
	k := newKeyedMutex()
	unlock := k.lock("owner")

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		k.lock("owner")()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock on the same key acquired while the first is held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	waitOrFail(t, acquired, "second lock after unlock")
}

func TestKeyedMutexIndependentKeys(t *testing.T) {
	k := newKeyedMutex()
	unlock := k.lock("a")
	defer unlock()

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		k.lock("b")()
	}()
	waitOrFail(t, acquired, "lock on another key")
}

func TestKeyedMutexDedup(t *testing.T) {
	k := newKeyedMutex()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Locking the same key twice in one call would deadlock without
		// dedup.
		k.lock("a", "b", "a", "b")()
	}()
	waitOrFail(t, done, "lock with duplicate keys")

	if len(k.locks) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(k.locks))
	}
}

func TestKeyedMutexUnlockIdempotent(t *testing.T) {
	k := newKeyedMutex()
	unlock := k.lock("a")
	unlock()
	unlock() // must not panic with "unlock of unlocked mutex"

	done := make(chan struct{})
	go func() {
		defer close(done)
		k.lock("a")()
	}()
	waitOrFail(t, done, "lock after double unlock")
}

// TestKeyedMutexOppositeOrder takes overlapping key sets given in opposite
// orders from many goroutines. Without the sorted acquisition order this
// deadlocks quickly; with it every goroutine finishes.
func TestKeyedMutexOppositeOrder(t *testing.T) {
	k := newKeyedMutex()
	var wg sync.WaitGroup
	var inside atomic.Int32
	keySets := [][]string{{"a", "b", "c"}, {"c", "b", "a"}, {"b", "a"}, {"c", "a"}}

	for i := range 40 {
		wg.Add(1)
		go func(keys []string) {
			defer wg.Done()
			for range 50 {
				unlock := k.lock(keys...)
				// All sets share "a", so at most one goroutine is ever here.
				if n := inside.Add(1); n != 1 {
					t.Errorf("%d goroutines inside the critical section", n)
				}
				inside.Add(-1)
				unlock()
			}
		}(keySets[i%len(keySets)])
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: goroutines locking overlapping keys in opposite orders did not finish")
	}
}

func TestKeyedMutexDoesNotModifyArgs(t *testing.T) {
	k := newKeyedMutex()
	keys := []string{"c", "a", "c"}
	k.lock(keys...)()
	if keys[0] != "c" || keys[1] != "a" || keys[2] != "c" {
		t.Fatalf("lock modified its arguments: %v", keys)
	}
}

// TestLockMembershipsForTxn checks the per-transaction bookkeeping: the locks
// stay held until the transaction is released, a second call on the same
// transaction does not deadlock on itself, and a call that would need new
// keys is refused instead of breaking the lock order.
func TestLockMembershipsForTxn(t *testing.T) {
	txn := &sql.Tx{}
	if !lockMembershipsForTxn(txn, []string{"tf_owner_a", "tf_owner_b"}) {
		t.Fatal("first lock refused")
	}

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		membershipLocks.lock("tf_owner_b")()
	}()
	select {
	case <-acquired:
		t.Fatal("membership lock not held by the transaction")
	case <-time.After(50 * time.Millisecond):
	}

	if !lockMembershipsForTxn(txn, []string{"tf_owner_a"}) {
		t.Fatal("second lock on a subset of the held roles refused")
	}
	if lockMembershipsForTxn(txn, []string{"tf_owner_c"}) {
		t.Fatal("second lock on a new role accepted")
	}

	releaseMembershipLocks(txn)
	waitOrFail(t, acquired, "lock after the transaction released it")

	// Releasing again (deferredRollback after commitTxn) is a no-op.
	releaseMembershipLocks(txn)
	if _, ok := txnMembershipUnlocks.Load(txn); ok {
		t.Fatal("transaction still registered after release")
	}
}

// TestLockDatabaseMemberships checks that the postgresql_database operations
// and withRolesGranted transactions exclude each other on the same role
// (#152), and that an empty role name is ignored.
func TestLockDatabaseMemberships(t *testing.T) {
	unlock := lockDatabaseMemberships("tf_db_owner_old", "", "tf_db_owner_new")

	txn := &sql.Tx{}
	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		lockMembershipsForTxn(txn, []string{"tf_db_owner_new"})
		releaseMembershipLocks(txn)
	}()
	select {
	case <-acquired:
		t.Fatal("withRolesGranted lock acquired while a database operation holds it")
	case <-time.After(50 * time.Millisecond):
	}

	// The empty name was not locked.
	membershipLocks.lock("")()

	unlock()
	waitOrFail(t, acquired, "withRolesGranted lock after the database operation released it")

	// The other way round.
	if !lockMembershipsForTxn(txn, []string{"tf_db_owner_old"}) {
		t.Fatal("lock refused")
	}
	acquired = make(chan struct{})
	go func() {
		defer close(acquired)
		lockDatabaseMemberships("tf_db_owner_old")()
	}()
	select {
	case <-acquired:
		t.Fatal("database operation lock acquired while a transaction holds it")
	case <-time.After(50 * time.Millisecond):
	}
	releaseMembershipLocks(txn)
	waitOrFail(t, acquired, "database operation lock after the transaction released it")
}
