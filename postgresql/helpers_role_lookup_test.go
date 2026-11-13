package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func openTestRoleLookupDB(t *testing.T) (*sql.DB, *sql.Tx) {
	t.Helper()
	config := getTestConfig(t)
	db, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		t.Fatalf("could not open connection: %v", err)
	}
	txn, err := db.Begin()
	if err != nil {
		closeDB(t, db)
		t.Fatalf("could not start transaction: %v", err)
	}
	return db, txn
}

func testRoleLookupName(t *testing.T) (existing, missing string) {
	suffix := strconv.Itoa(int(time.Now().UnixNano()))
	existing = fmt.Sprintf("%s_lookup_%s", roleNamePrefix, suffix)
	missing = fmt.Sprintf("%s_missing_%s", roleNamePrefix, suffix)
	return
}

func TestRoleOIDAndExists(t *testing.T) {
	skipIfNotAcc(t)
	existing, missing := testRoleLookupName(t)
	defer createTestRole(t, existing)()

	db, txn := openTestRoleLookupDB(t)
	defer closeDB(t, db)
	defer deferredRollback(txn)

	var expectedOID uint32
	if err := txn.QueryRow("SELECT oid FROM pg_roles WHERE rolname = $1", existing).Scan(&expectedOID); err != nil {
		t.Fatalf("could not read oid of %s: %v", existing, err)
	}

	oid, exists, err := roleOIDAndExists(txn, existing)
	if err != nil || !exists || oid != expectedOID {
		t.Fatalf("roleOIDAndExists(%s) = (%d, %v, %v), want (%d, true, nil)", existing, oid, exists, err, expectedOID)
	}

	oid, exists, err = roleOIDAndExists(txn, missing)
	if err != nil || exists || oid != 0 {
		t.Fatalf("roleOIDAndExists(%s) = (%d, %v, %v), want (0, false, nil)", missing, oid, exists, err)
	}
}

// getRoleOID and roleExists are thin wrappers around roleOIDAndExists and must
// keep their historical results and error texts.
func TestRoleOIDWrappersKeepBehavior(t *testing.T) {
	skipIfNotAcc(t)
	existing, missing := testRoleLookupName(t)
	defer createTestRole(t, existing)()

	db, txn := openTestRoleLookupDB(t)
	defer closeDB(t, db)
	defer deferredRollback(txn)

	if oid, err := getRoleOID(txn, publicRole); err != nil || oid != 0 {
		t.Fatalf("getRoleOID(public) = (%d, %v), want (0, nil)", oid, err)
	}
	if oid, err := getRoleOID(txn, existing); err != nil || oid == 0 {
		t.Fatalf("getRoleOID(%s) = (%d, %v), want non-zero oid", existing, oid, err)
	}
	_, err := getRoleOID(txn, missing)
	if err == nil || !errors.Is(err, sql.ErrNoRows) ||
		err.Error() != fmt.Sprintf("could not find oid for role %s: %v", missing, sql.ErrNoRows) {
		t.Fatalf("getRoleOID(%s) error = %v, want 'could not find oid for role ...: sql: no rows in result set'", missing, err)
	}

	for role, want := range map[string]bool{existing: true, missing: false, publicRole: false} {
		got, err := roleExists(txn, role)
		if err != nil || got != want {
			t.Fatalf("roleExists(%s) = (%v, %v), want (%v, nil)", role, got, err, want)
		}
	}
}
