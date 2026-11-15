package postgresql

import (
	"fmt"
	"log"
	"strings"

	"github.com/lib/pq"
)

// roleMembershipState is what withRolesGranted needs to know about the
// relationship between the current user and one role it wants to act as.
type roleMembershipState struct {
	// member: the current user can already act as the role (has its
	// privileges and, on PostgreSQL 16+, may SET ROLE to it), so no
	// temporary membership is needed.
	member bool
	// canAdmin: the current user may grant the role (ADMIN OPTION on it, or
	// CREATEROLE before PostgreSQL 16).
	canAdmin bool
	// reverseDirect: the role is a direct member of the current user.
	reverseDirect bool
	// reverseIndirect: the role is a member of the current user through
	// another role. Revoking a direct row does not break that cycle, so the
	// current user cannot become a member of the role.
	reverseIndirect bool
}

// membershipAction is what withRolesGranted does for one role.
type membershipAction int

const (
	// membershipSkip: nothing to do, the current user can act as the role.
	membershipSkip membershipAction = iota
	// membershipGrant: revoke the direct reverse membership if any, then
	// grant the role to the current user, as the provider always did.
	membershipGrant
	// membershipTryGrant: same as membershipGrant, but the catalog says it
	// will probably fail (no ADMIN OPTION on the role, or a reverse
	// membership the current user may not be able to revoke): run it under
	// a savepoint and fall back to a direct grant if it fails.
	membershipTryGrant
	// membershipBlocked: the role is a member of the current user through
	// another role; the current user cannot become a member of it.
	membershipBlocked
)

func (s roleMembershipState) action() membershipAction {
	switch {
	case s.member:
		return membershipSkip
	case s.reverseIndirect:
		return membershipBlocked
	case !s.canAdmin || s.reverseDirect:
		return membershipTryGrant
	default:
		return membershipGrant
	}
}

// roleMembershipStatesQuery returns the query reading roleMembershipState for
// a list of roles ($2) and the current user ($1) in one round-trip. pg_has_role
// follows memberships recursively, unlike a lookup of pg_auth_members rows.
//
// member keeps the provider's former test (a direct pg_auth_members row, with
// set_option since PostgreSQL 16) and adds indirect memberships that give both
// the role's privileges and, since 16, SET ROLE.
func roleMembershipStatesQuery(pg16 bool) string {
	member := `pg_has_role(cu.oid, r.oid, 'USAGE')
    OR EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid AND m.member = cu.oid)`
	canAdmin := `cu.rolcreaterole OR pg_has_role(cu.oid, r.oid, 'MEMBER WITH ADMIN OPTION')`
	if pg16 {
		member = `(pg_has_role(cu.oid, r.oid, 'USAGE') AND pg_has_role(cu.oid, r.oid, 'SET'))
    OR EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid AND m.member = cu.oid AND m.set_option)`
		// Since PostgreSQL 16 CREATEROLE no longer allows granting any role.
		canAdmin = `pg_has_role(cu.oid, r.oid, 'MEMBER WITH ADMIN OPTION')`
	}
	return fmt.Sprintf(`
SELECT r.rolname,
  %s AS member,
  %s AS can_admin,
  EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = cu.oid AND m.member = r.oid) AS reverse_direct,
  EXISTS (
    SELECT 1 FROM pg_auth_members m
    WHERE m.member = r.oid AND m.roleid <> cu.oid AND pg_has_role(m.roleid, cu.oid, 'MEMBER')
  ) AS reverse_indirect
FROM pg_roles cu, pg_roles r
WHERE cu.rolname = $1 AND r.rolname = ANY($2)`, member, canAdmin)
}

// getRoleMembershipStates reads roleMembershipState of roles for currentUser.
// Roles that do not exist are absent from the result.
func getRoleMembershipStates(db QueryAble, pg16 bool, currentUser string, roles []string) (map[string]roleMembershipState, error) {
	rows, err := db.Query(roleMembershipStatesQuery(pg16), currentUser, pq.Array(roles))
	if err != nil {
		return nil, fmt.Errorf("could not read memberships of %s in roles %v: %w", currentUser, roles, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("[WARN] could not close rows while reading memberships: %v", err)
		}
	}()

	states := make(map[string]roleMembershipState, len(roles))
	for rows.Next() {
		var role string
		var s roleMembershipState
		if err := rows.Scan(&role, &s.member, &s.canAdmin, &s.reverseDirect, &s.reverseIndirect); err != nil {
			return nil, fmt.Errorf("could not scan role membership: %w", err)
		}
		states[role] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("could not read memberships of %s in roles %v: %w", currentUser, roles, err)
	}
	return states, nil
}

// membershipChainQuery finds the shortest chain of memberships from $1 up to
// $2 (member, the role it is a member of, the role that one is a member of,
// ...). Only used to explain an error.
const membershipChainQuery = `
WITH RECURSIVE chain(roleid, path) AS (
  SELECT m.roleid, ARRAY[m.member, m.roleid]
  FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member
  WHERE r.rolname = $1
  UNION ALL
  SELECT m.roleid, c.path || m.roleid
  FROM chain c JOIN pg_auth_members m ON m.member = c.roleid
  WHERE NOT m.roleid = ANY(c.path)
)
SELECT ARRAY(
  SELECT pg_get_userbyid(u.id)
  FROM unnest((
    SELECT c.path FROM chain c JOIN pg_roles r ON r.oid = c.roleid
    WHERE r.rolname = $2
    ORDER BY array_length(c.path, 1)
    LIMIT 1
  )) WITH ORDINALITY AS u(id, ord)
  ORDER BY u.ord
)`

// membershipChain returns the shortest membership chain from member up to
// role, e.g. [a b rds] when a is a member of b and b of rds.
func membershipChain(db QueryAble, member, role string) ([]string, error) {
	var chain pq.StringArray
	if err := db.QueryRow(membershipChainQuery, member, role).Scan(&chain); err != nil {
		return nil, fmt.Errorf("could not read the membership chain from %s to %s: %w", member, role, err)
	}
	return chain, nil
}

// blockedRole is a role withRolesGranted could not make the current user a
// member of, and why.
type blockedRole struct {
	role   string
	reason string
	// circular: the role is a member of the current user, so granting it to
	// the current user is impossible, not only forbidden to it.
	circular bool
	// superuser: the role is a superuser, which the provider cannot grant
	// to a non-superuser current user.
	superuser bool
}

// blockedRolesError explains why the current user could not act as the
// blocked roles and what would fix it.
func blockedRolesError(currentUser string, blocked []blockedRole, directGrant bool) error {
	var reasons, names, grantable []string
	circular := false
	for _, b := range blocked {
		reasons = append(reasons, b.reason)
		names = append(names, pq.QuoteIdentifier(b.role))
		switch {
		case b.circular:
			circular = true
		case b.superuser:
			// Only the direct grant can fix it.
		default:
			grantable = append(grantable, pq.QuoteIdentifier(b.role))
		}
	}

	var fixes []string
	if len(grantable) > 0 {
		fixes = append(fixes, fmt.Sprintf("grant %s to %s", strings.Join(grantable, ", "), pq.QuoteIdentifier(currentUser)))
	}
	if circular {
		fixes = append(fixes, "remove the circular membership")
	}
	if directGrant {
		fixes = append(fixes, fmt.Sprintf("grant the managed privileges to %s WITH GRANT OPTION on every target object", pq.QuoteIdentifier(currentUser)))
	}

	return fmt.Errorf(
		"current user %s cannot act as %s: %s; to fix it, %s",
		pq.QuoteIdentifier(currentUser), strings.Join(names, ", "), strings.Join(reasons, "; "), strings.Join(fixes, ", or "),
	)
}

// takeMembership grants role to currentUser, first revoking currentUser from
// role when role is a direct member of it (circular memberships are refused).
func takeMembership(txn QueryAble, currentUser, role string, revokeReverse bool) error {
	if revokeReverse {
		log.Printf("withRolesGranted: revoking %s from %s", currentUser, role)
		query := fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(currentUser), pq.QuoteIdentifier(role))
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("error revoking role %s from %s: %w", currentUser, role, err)
		}
	}
	log.Printf("withRolesGranted: granting %s to %s", role, currentUser)
	query := fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(role), pq.QuoteIdentifier(currentUser))
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("error granting role %s to %s: %w", role, currentUser, err)
	}
	return nil
}

const membershipSavepoint = "tf_with_roles_granted"

// tryTakeMembership is takeMembership under a savepoint: when it fails, the
// transaction is rolled back to the savepoint and stays usable, and the
// failure is returned as grantErr. err is set when the transaction itself
// could not be kept usable.
func tryTakeMembership(txn QueryAble, currentUser, role string, revokeReverse bool) (grantErr, err error) {
	if _, err := txn.Exec("SAVEPOINT " + membershipSavepoint); err != nil {
		return nil, fmt.Errorf("could not create savepoint: %w", err)
	}
	if grantErr := takeMembership(txn, currentUser, role, revokeReverse); grantErr != nil {
		if _, err := txn.Exec("ROLLBACK TO SAVEPOINT " + membershipSavepoint); err != nil {
			return nil, fmt.Errorf("%w (and could not roll back to savepoint: %w)", grantErr, err)
		}
		return grantErr, nil
	}
	if _, err := txn.Exec("RELEASE SAVEPOINT " + membershipSavepoint); err != nil {
		return nil, fmt.Errorf("could not release savepoint: %w", err)
	}
	return nil, nil
}
