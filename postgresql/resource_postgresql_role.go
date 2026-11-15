package postgresql

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lib/pq"
)

const (
	roleBypassRLSAttr                       = "bypass_row_level_security"
	roleConnLimitAttr                       = "connection_limit"
	roleCreateDBAttr                        = "create_database"
	roleCreateRoleAttr                      = "create_role"
	roleEncryptedPassAttr                   = "encrypted_password"
	roleIdleInTransactionSessionTimeoutAttr = "idle_in_transaction_session_timeout"
	roleInheritAttr                         = "inherit"
	roleLoginAttr                           = "login"
	roleNameAttr                            = "name"
	rolePasswordAttr                        = "password"
	rolePasswordWOAttr                      = "password_wo"
	rolePasswordWOVersionAttr               = "password_wo_version"
	roleReplicationAttr                     = "replication"
	roleSkipDropRoleAttr                    = "skip_drop_role"
	roleSkipReassignOwnedAttr               = "skip_reassign_owned"
	roleSuperuserAttr                       = "superuser"
	roleValidUntilAttr                      = "valid_until"
	roleRolesAttr                           = "roles"
	roleSearchPathAttr                      = "search_path"
	roleStatementTimeoutAttr                = "statement_timeout"
	roleAssumeRoleAttr                      = "assume_role"

	// Deprecated options
	roleDepEncryptedAttr = "encrypted"
)

func resourcePostgreSQLRole() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceClusterWriteFunc(resourcePostgreSQLRoleCreate),
		Read:   PGResourceFunc(resourcePostgreSQLRoleRead),
		Update: PGResourceClusterWriteFunc(resourcePostgreSQLRoleUpdate),
		Delete: PGResourceClusterWriteFunc(resourcePostgreSQLRoleDelete),
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		SchemaVersion: 1,
		StateUpgraders: []schema.StateUpgrader{
			{
				Version: 0,
				Type:    resourcePostgreSQLRoleV0().CoreConfigSchema().ImpliedType(),
				Upgrade: upgradeRoleStateV0,
			},
		},

		Schema: roleSchema(),
	}
}

func roleSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		roleNameAttr: {
			Type:        schema.TypeString,
			Required:    true,
			Description: "The name of the role",
		},
		rolePasswordAttr: {
			Type:          schema.TypeString,
			Optional:      true,
			Sensitive:     true,
			ConflictsWith: []string{rolePasswordWOAttr, rolePasswordWOVersionAttr},
			Description:   "Sets the role's password",
		},
		rolePasswordWOAttr: {
			Type:          schema.TypeString,
			Optional:      true,
			Sensitive:     true,
			ConflictsWith: []string{rolePasswordAttr},
			RequiredWith:  []string{rolePasswordWOVersionAttr},
			WriteOnly:     true,
			Description:   "Sets the role's password without storing it in the state file.",
		},
		rolePasswordWOVersionAttr: {
			Type:          schema.TypeString,
			Optional:      true,
			ConflictsWith: []string{rolePasswordAttr},
			RequiredWith:  []string{rolePasswordWOAttr},
			Description:   "Prevents applies from updating the role password on every apply unless the value changes.",
		},
		roleDepEncryptedAttr: {
			Type:       schema.TypeString,
			Optional:   true,
			Deprecated: fmt.Sprintf("Rename PostgreSQL role resource attribute %q to %q", roleDepEncryptedAttr, roleEncryptedPassAttr),
		},
		roleRolesAttr: {
			Type:        schema.TypeSet,
			Optional:    true,
			Elem:        &schema.Schema{Type: schema.TypeString},
			Set:         schema.HashString,
			MinItems:    0,
			Description: "Role(s) to grant to this new role",
		},
		roleSearchPathAttr: {
			Type:        schema.TypeList,
			Optional:    true,
			Elem:        &schema.Schema{Type: schema.TypeString},
			MinItems:    0,
			Description: "Sets the role's search path",
		},
		roleEncryptedPassAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     true,
			Description: "Control whether the password is stored encrypted in the system catalogs",
		},
		roleValidUntilAttr: {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "infinity",
			Description: "Sets a date and time after which the role's password is no longer valid",
		},
		roleConnLimitAttr: {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      -1,
			Description:  "How many concurrent connections can be made with this role",
			ValidateFunc: validation.IntAtLeast(-1),
		},
		roleSuperuserAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: `Determine whether the new role is a "superuser"`,
		},
		roleCreateDBAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Define a role's ability to create databases",
		},
		roleCreateRoleAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Determine whether this role will be permitted to create new roles",
		},
		roleIdleInTransactionSessionTimeoutAttr: {
			Type:             schema.TypeString,
			Optional:         true,
			Description:      "Terminate any session with an open transaction that has been idle for longer than the specified duration: milliseconds, or a number with one of the units us, ms, s, min, h, d. 0 disables the timeout for the role; leave it unset to inherit the database or server value",
			ValidateFunc:     validateMillisecondsSetting,
			DiffSuppressFunc: suppressEquivalentMilliseconds,
		},
		roleInheritAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     true,
			Description: `Determine whether a role "inherits" the privileges of roles it is a member of`,
		},
		roleLoginAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Determine whether a role is allowed to log in",
		},
		roleReplicationAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Determine whether a role is allowed to initiate streaming replication or put the system in and out of backup mode",
		},
		roleBypassRLSAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Determine whether a role bypasses every row-level security (RLS) policy",
		},
		roleSkipDropRoleAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Skip actually running the DROP ROLE command when removing a ROLE from PostgreSQL",
		},
		roleSkipReassignOwnedAttr: {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Skip actually running the REASSIGN OWNED command when removing a role from PostgreSQL",
		},
		roleStatementTimeoutAttr: {
			Type:             schema.TypeString,
			Optional:         true,
			Description:      "Abort any statement that takes more than the specified duration: milliseconds, or a number with one of the units us, ms, s, min, h, d. 0 disables the timeout for the role; leave it unset to inherit the database or server value",
			ValidateFunc:     validateMillisecondsSetting,
			DiffSuppressFunc: suppressEquivalentMilliseconds,
		},
		roleAssumeRoleAttr: {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "Role to switch to at login",
		},
	}
}

// roleTimeoutAttrs are the millisecond-based settings of the role. They are
// strings: unset resets the setting, so the role inherits the database or
// server value, and "0" sets it to 0, which disables the timeout.
var roleTimeoutAttrs = []string{roleStatementTimeoutAttr, roleIdleInTransactionSessionTimeoutAttr}

// resourcePostgreSQLRoleV0 is version 0 of the postgresql_role schema, where
// the timeouts were numbers and 0 meant unset.
func resourcePostgreSQLRoleV0() *schema.Resource {
	s := roleSchema()
	for _, attr := range roleTimeoutAttrs {
		s[attr] = &schema.Schema{Type: schema.TypeInt, Optional: true}
	}
	return &schema.Resource{Schema: s}
}

// upgradeRoleStateV0 turns the timeouts into strings. A 0 in version 0 was
// what Read stored for a role without the setting, so it becomes unset.
func upgradeRoleStateV0(_ context.Context, rawState map[string]any, _ any) (map[string]any, error) {
	for _, attr := range roleTimeoutAttrs {
		var ms int64
		switch v := rawState[attr].(type) {
		case nil, string:
			continue
		case float64:
			ms = int64(v)
		case int:
			ms = int64(v)
		case int64:
			ms = v
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return nil, fmt.Errorf("could not upgrade %s %q: %w", attr, v, err)
			}
			ms = n
		default:
			return nil, fmt.Errorf("could not upgrade %s: unexpected type %T", attr, v)
		}
		if ms == 0 {
			delete(rawState, attr)
		} else {
			rawState[attr] = strconv.FormatInt(ms, 10)
		}
	}
	return rawState, nil
}

func resourcePostgreSQLRoleCreate(db *DBConnection, d *schema.ResourceData) error {
	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	stringOpts := []struct {
		hclKey string
		sqlKey string
	}{
		{roleValidUntilAttr, "VALID UNTIL"}, // always keep this
	}

	// Decide which password attribute (if any) is set and prepend it.
	// NOTE: getWO returns (string, bool) just like d.GetOk.
	if v, ok := d.GetOk(rolePasswordAttr); ok && v.(string) != "" {
		// normal password stored in state
		stringOpts = append(
			[]struct{ hclKey, sqlKey string }{{rolePasswordAttr, "PASSWORD"}},
			stringOpts...,
		)
	} else if _, ok := getWO(d, rolePasswordWOAttr); ok {
		// write-only password
		stringOpts = append(
			[]struct{ hclKey, sqlKey string }{{rolePasswordWOAttr, "PASSWORD"}},
			stringOpts...,
		)
	}

	intOpts := []struct {
		hclKey string
		sqlKey string
	}{
		{roleConnLimitAttr, "CONNECTION LIMIT"},
	}

	type boolOptType struct {
		hclKey        string
		sqlKeyEnable  string
		sqlKeyDisable string
	}
	boolOpts := []boolOptType{
		{roleSuperuserAttr, "SUPERUSER", "NOSUPERUSER"},
		{roleCreateDBAttr, "CREATEDB", "NOCREATEDB"},
		{roleCreateRoleAttr, "CREATEROLE", "NOCREATEROLE"},
		{roleInheritAttr, "INHERIT", "NOINHERIT"},
		{roleLoginAttr, "LOGIN", "NOLOGIN"},
		// roleEncryptedPassAttr is used only when rolePasswordAttr is set.
		// {roleEncryptedPassAttr, "ENCRYPTED", "UNENCRYPTED"},
	}

	if db.featureSupported(featureRLS) {
		boolOpts = append(boolOpts, boolOptType{roleBypassRLSAttr, "BYPASSRLS", "NOBYPASSRLS"})
	}

	if db.featureSupported(featureReplication) {
		boolOpts = append(boolOpts, boolOptType{roleReplicationAttr, "REPLICATION", "NOREPLICATION"})
	}

	createOpts := make([]string, 0, len(stringOpts)+len(intOpts)+len(boolOpts))

	for _, opt := range stringOpts {
		var val string
		var ok bool

		if opt.hclKey == rolePasswordWOAttr {
			v, found := getWO(d, opt.hclKey)
			if found {
				val = v
				if val != "" {
					ok = true
				}
			}
		} else {
			v, found := d.GetOk(opt.hclKey)
			if found {
				val = v.(string)
				if val != "" {
					ok = true
				}
			}
		}

		if !ok {
			continue
		}

		switch opt.hclKey {
		case rolePasswordWOAttr, rolePasswordAttr:
			if strings.ToUpper(val) == "NULL" {
				createOpts = append(createOpts, "PASSWORD NULL")
			} else {
				if d.Get(roleEncryptedPassAttr).(bool) {
					createOpts = append(createOpts, "ENCRYPTED")
				} else {
					createOpts = append(createOpts, "UNENCRYPTED")
				}
				createOpts = append(createOpts, rolePasswordClause(val))
			}

		case roleValidUntilAttr:
			if val == "" || strings.ToLower(val) == "infinity" {
				createOpts = append(createOpts,
					fmt.Sprintf("%s 'infinity'", opt.sqlKey))
			} else {
				createOpts = append(createOpts,
					fmt.Sprintf("%s %s", opt.sqlKey, pq.QuoteLiteral(val)))
			}

		default:
			createOpts = append(createOpts,
				fmt.Sprintf("%s %s", opt.sqlKey, pq.QuoteIdentifier(val)))
		}
	}

	for _, opt := range intOpts {
		val := d.Get(opt.hclKey).(int)
		createOpts = append(createOpts, fmt.Sprintf("%s %d", opt.sqlKey, val))
	}

	for _, opt := range boolOpts {
		if opt.hclKey == roleEncryptedPassAttr {
			// This attribute is handled above in the stringOpts
			// loop.
			continue
		}
		val := d.Get(opt.hclKey).(bool)
		valStr := opt.sqlKeyDisable
		if val {
			valStr = opt.sqlKeyEnable
		}
		createOpts = append(createOpts, valStr)
	}

	roleName := d.Get(roleNameAttr).(string)
	createStr := strings.Join(createOpts, " ")
	if len(createOpts) > 0 {
		if db.featureSupported(featureCreateRoleWith) {
			createStr = " WITH " + createStr
		} else {
			// NOTE(seanc@): Work around ParAccel/AWS RedShift's ancient fork of PostgreSQL
			createStr = " " + createStr
		}
	}

	sql := fmt.Sprintf("CREATE ROLE %s%s", pq.QuoteIdentifier(roleName), createStr)

	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error creating role %s: %w", roleName, err)
	}

	if err = grantRoles(txn, d); err != nil {
		return err
	}

	if err = alterSearchPath(txn, d); err != nil {
		return err
	}

	for _, attr := range roleTimeoutAttrs {
		if err = setRoleTimeout(txn, d, attr); err != nil {
			return err
		}
	}

	if err = setAssumeRole(txn, d); err != nil {
		return err
	}

	if err = txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}

	d.SetId(roleName)

	return resourcePostgreSQLRoleReadImpl(db, d)
}

func resourcePostgreSQLRoleDelete(db *DBConnection, d *schema.ResourceData) error {
	roleName := d.Get(roleNameAttr).(string)
	skipReassignOwned := d.Get(roleSkipReassignOwnedAttr).(bool)

	// REASSIGN OWNED and DROP OWNED only act on the current database (and on
	// shared objects), so they first run in every other database where the
	// role owns objects or holds privileges, one database at a time (#137).
	if !skipReassignOwned {
		if err := reassignOwnedInOtherDatabases(db, roleName); err != nil {
			return err
		}
	}

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, roleName); err != nil {
		return err
	}

	if !skipReassignOwned {
		if err := reassignAndDropOwned(db, txn, roleName); err != nil {
			return err
		}
	}
	if !d.Get(roleSkipDropRoleAttr).(bool) {
		if _, err := txn.Exec(fmt.Sprintf("DROP ROLE %s", pq.QuoteIdentifier(roleName))); err != nil {
			return fmt.Errorf("could not delete role %s: %w", roleName, err)
		}
	}

	if err := commitTxn(txn); err != nil {
		return fmt.Errorf("error committing schema: %w", err)
	}

	d.SetId("")

	return nil
}

// reassignAndDropOwned gives the objects role owns in the database of txn (and
// the shared objects it owns) to the provider user, then drops the privileges
// role still holds there. The caller holds pgLockRole on txn.
func reassignAndDropOwned(db *DBConnection, txn *sql.Tx, role string) error {
	return withRolesGranted(db, txn, []string{role}, func() error {
		currentUser := db.client.config.getDatabaseUsername()
		if _, err := txn.Exec(fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pq.QuoteIdentifier(role), pq.QuoteIdentifier(currentUser))); err != nil {
			return fmt.Errorf("could not reassign owned by role %s to %s: %w", role, currentUser, err)
		}

		if _, err := txn.Exec(fmt.Sprintf("DROP OWNED BY %s", pq.QuoteIdentifier(role))); err != nil {
			return fmt.Errorf("could not drop owned by role %s: %w", role, err)
		}
		return nil
	})
}

// roleDependentDatabasesQuery lists the databases, other than the current one,
// that hold objects owned by role or privileges granted to it. pg_shdepend is
// a shared catalog readable by any user, so this needs no connection to these
// databases. Dependencies on shared objects (dbid = 0) are handled by
// REASSIGN/DROP OWNED in the provider's database. Databases that do not allow
// connections (template0) are left out: DROP ROLE then reports what is left.
const roleDependentDatabasesQuery = `SELECT DISTINCT d.datname
FROM pg_shdepend s
JOIN pg_roles r ON r.oid = s.refobjid
JOIN pg_database d ON d.oid = s.dbid
WHERE s.refclassid = 'pg_authid'::regclass
  AND r.rolname = $1
  AND d.datname <> current_database()
  AND d.datallowconn
ORDER BY d.datname`

func roleDependentDatabases(db QueryAble, role string) ([]string, error) {
	rows, err := db.Query(roleDependentDatabasesQuery, role)
	if err != nil {
		return nil, fmt.Errorf("could not list the databases where role %s owns objects: %w", role, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("[ERR] could not close rows: %v", err)
		}
	}()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("could not scan database name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// reassignOwnedInOtherDatabases runs REASSIGN OWNED / DROP OWNED for role in
// every database other than the provider's one where role owns objects or
// holds privileges.
//
// Each database gets its own transaction on that database's pool (shared with
// the other resources, so max_idle_connections and the eviction before DROP
// DATABASE apply), committed before the next one starts: at most one
// connection is held at any time, so no pool is ever asked for a second
// connection while one of its connections is pinned by this operation. This
// also keeps the lock order of withRolesGranted (advisory locks first, then
// the membership locks until the transaction ends).
//
// A database dropped since the catalog was read is skipped.
func reassignOwnedInOtherDatabases(db *DBConnection, role string) error {
	dbNames, err := roleDependentDatabases(db, role)
	if err != nil {
		return err
	}

	for _, dbName := range dbNames {
		if dbName == db.client.databaseName {
			continue
		}
		if err := reassignOwnedInDatabase(db.client, dbName, role); err != nil {
			if isDatabaseDoesNotExistError(err) {
				log.Printf("[DEBUG] database %s no longer exists, skipping REASSIGN OWNED BY %s there", dbName, role)
				continue
			}
			return fmt.Errorf("in database %s: %w", dbName, err)
		}
	}
	return nil
}

func reassignOwnedInDatabase(client *Client, dbName, role string) error {
	dbConn, err := initConnection(client, dbName)
	if err != nil {
		return err
	}

	txn, err := dbConn.Begin()
	if err != nil {
		return fmt.Errorf("could not start transaction: %w", err)
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, role); err != nil {
		return err
	}
	if err := reassignAndDropOwned(dbConn, txn, role); err != nil {
		return err
	}
	return commitTxn(txn)
}

func resourcePostgreSQLRoleRead(db *DBConnection, d *schema.ResourceData) error {
	return resourcePostgreSQLRoleReadImpl(db, d)
}

func resourcePostgreSQLRoleReadImpl(db *DBConnection, d *schema.ResourceData) error {
	var roleSuperuser, roleInherit, roleCreateRole, roleCreateDB, roleCanLogin, roleReplication, roleBypassRLS bool
	var roleConnLimit int
	var roleName, roleValidUntil string
	var roleRoles, roleConfig pq.ByteaArray

	roleID := d.Id()

	columns := []string{
		"rolname",
		"rolsuper",
		"rolinherit",
		"rolcreaterole",
		"rolcreatedb",
		"rolcanlogin",
		"rolconnlimit",
		`COALESCE(rolvaliduntil::TEXT, 'infinity')`,
		"rolconfig",
	}

	values := []any{
		&roleRoles,
		&roleName,
		&roleSuperuser,
		&roleInherit,
		&roleCreateRole,
		&roleCreateDB,
		&roleCanLogin,
		&roleConnLimit,
		&roleValidUntil,
		&roleConfig,
	}

	if db.featureSupported(featureReplication) {
		columns = append(columns, "rolreplication")
		values = append(values, &roleReplication)
	}

	if db.featureSupported(featureRLS) {
		columns = append(columns, "rolbypassrls")
		values = append(values, &roleBypassRLS)
	}

	roleSQL := fmt.Sprintf(`SELECT ARRAY(
			SELECT pg_get_userbyid(roleid) FROM pg_catalog.pg_auth_members members WHERE member = pg_roles.oid
		), %s
		FROM pg_catalog.pg_roles WHERE rolname=$1`,
		// select columns
		strings.Join(columns, ", "),
	)
	err := db.QueryRow(roleSQL, roleID).Scan(values...)

	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL ROLE (%s) not found", roleID)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading ROLE: %w", err)
	}

	d.Set(roleNameAttr, roleName)
	d.Set(roleConnLimitAttr, roleConnLimit)
	d.Set(roleCreateDBAttr, roleCreateDB)
	d.Set(roleCreateRoleAttr, roleCreateRole)
	d.Set(roleEncryptedPassAttr, true)
	d.Set(roleInheritAttr, roleInherit)
	d.Set(roleLoginAttr, roleCanLogin)
	d.Set(roleSkipDropRoleAttr, d.Get(roleSkipDropRoleAttr).(bool))
	d.Set(roleSkipReassignOwnedAttr, d.Get(roleSkipReassignOwnedAttr).(bool))
	d.Set(roleSuperuserAttr, roleSuperuser)
	d.Set(roleValidUntilAttr, roleValidUntil)
	d.Set(roleReplicationAttr, roleReplication)
	d.Set(roleBypassRLSAttr, roleBypassRLS)
	d.Set(roleRolesAttr, pgArrayToSet(roleRoles))
	d.Set(roleSearchPathAttr, readSearchPath(roleConfig))
	d.Set(roleAssumeRoleAttr, readAssumeRole(roleConfig))

	for _, attr := range roleTimeoutAttrs {
		value, err := readRoleTimeout(roleConfig, attr, d.Get(attr).(string))
		if err != nil {
			return err
		}
		d.Set(attr, value)
	}

	d.SetId(roleName)

	if _, ok := d.GetOk(rolePasswordAttr); ok {
		password, err := readRolePassword(db, d, roleCanLogin)
		if err != nil {
			return err
		}
		d.Set(rolePasswordAttr, password)
	}
	return nil
}

// readSearchPath searches for a search_path entry in the rolconfig array.
// In case no such value is present, it returns nil.
func readSearchPath(roleConfig pq.ByteaArray) []string {
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, roleSearchPathAttr) {
			var result = strings.Split(strings.TrimPrefix(config, roleSearchPathAttr+"="), ", ")
			for i := range result {
				result[i] = strings.Trim(result[i], `"`)
			}
			return result
		}
	}
	return nil
}

// millisecondUnits maps the time units PostgreSQL accepts for
// millisecond-based settings to their value in milliseconds.
// https://www.postgresql.org/docs/current/config-setting.html#CONFIG-SETTING-NAMES-VALUES
var millisecondUnits = []struct {
	suffix string
	factor float64
}{
	// Longer suffixes first so e.g. "ms" is not matched as "s".
	{"ms", 1},
	{"us", 0.001},
	{"min", 60 * 1000},
	{"s", 1000},
	{"h", 60 * 60 * 1000},
	{"d", 24 * 60 * 60 * 1000},
}

// parseMillisecondsSetting converts a millisecond-based setting value to an
// integer number of milliseconds. PostgreSQL stores the value exactly as it
// was set, so alongside a bare integer ("120000") it can carry any of the
// documented time units ("2min", "5s", "150ms"), optionally with whitespace
// before the unit.
func parseMillisecondsSetting(value string) (int, error) {
	if res, err := strconv.Atoi(value); err == nil {
		return res, nil
	}
	for _, unit := range millisecondUnits {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
		res, err := strconv.ParseFloat(number, 64)
		if err != nil {
			break
		}
		return int(math.Round(res * unit.factor)), nil
	}
	return -1, fmt.Errorf("invalid millisecond setting value %q", value)
}

// readRoleTimeout returns the value of a millisecond-based setting in the
// rolconfig array, or "" when the role does not set it. When current (the
// value in the state) is the same duration, it is kept, so a value written in
// another unit outside Terraform (e.g. '12s' for 12000) shows no diff.
func readRoleTimeout(roleConfig pq.ByteaArray, attr, current string) (string, error) {
	for _, v := range roleConfig {
		config := string(v)
		if !strings.HasPrefix(config, attr+"=") {
			continue
		}
		value := strings.Split(strings.TrimPrefix(config, attr+"="), ", ")[0]
		ms, err := parseMillisecondsSetting(value)
		if err != nil {
			return "", fmt.Errorf("error reading %s: %w", attr, err)
		}
		if current != "" {
			if cur, err := parseMillisecondsSetting(current); err == nil && cur == ms {
				return current, nil
			}
		}
		return value, nil
	}
	return "", nil
}

// validateMillisecondsSetting accepts what PostgreSQL accepts for a
// millisecond-based setting: a number of milliseconds, or a number with one of
// the units us, ms, s, min, h, d. An empty string is the same as unset.
func validateMillisecondsSetting(v any, k string) ([]string, []error) {
	value := v.(string)
	if value == "" {
		return nil, nil
	}
	ms, err := parseMillisecondsSetting(value)
	if err != nil {
		return nil, []error{fmt.Errorf("%s: %w; use milliseconds or a number with one of the units us, ms, s, min, h, d", k, err)}
	}
	if ms < 0 {
		return nil, []error{fmt.Errorf("%s must not be negative, got %q", k, value)}
	}
	return nil, nil
}

// suppressEquivalentMilliseconds hides the diff between two spellings of the
// same duration (30000 and 30s). Unset and 0 stay different: unset inherits
// the database or server value, 0 disables the timeout.
func suppressEquivalentMilliseconds(_, old, new string, _ *schema.ResourceData) bool {
	if old == "" || new == "" {
		return old == new
	}
	o, err := parseMillisecondsSetting(old)
	if err != nil {
		return false
	}
	n, err := parseMillisecondsSetting(new)
	if err != nil {
		return false
	}
	return o == n
}

// readAssumeRole searches for a role entry in the rolconfig array.
// In case no such value is present, it returns empty string.
func readAssumeRole(roleConfig pq.ByteaArray) string {
	var res string
	var assumeRoleAttr = "role"
	for _, v := range roleConfig {
		config := string(v)
		if strings.HasPrefix(config, assumeRoleAttr) {
			res = strings.TrimPrefix(config, assumeRoleAttr+"=")
		}
	}
	return res
}

// readRolePassword reads password either from Postgres if admin user is a superuser
// or only from Terraform state.
func readRolePassword(db *DBConnection, d *schema.ResourceData, roleCanLogin bool) (string, error) {
	statePassword := d.Get(rolePasswordAttr).(string)

	// Role which cannot login does not have password in pg_shadow.
	// Also, if user specifies that admin is not a superuser we don't try to read pg_shadow
	// (only superuser can read pg_shadow)
	if !roleCanLogin || !db.client.config.Superuser {
		return statePassword, nil
	}

	// Otherwise we check if connected user is really a superuser
	// (in order to warn user instead of having a permission denied error)
	superuser, err := db.isSuperuser()
	if err != nil {
		return "", err
	}
	if !superuser {
		return "", fmt.Errorf(
			"could not read role password from Postgres as "+
				"connected user %s is not a SUPERUSER. "+
				"You can set `superuser = false` in the provider configuration "+
				"so it will not try to read the password from Postgres",
			db.client.config.getDatabaseUsername(),
		)
	}

	var rolePassword string
	err = db.QueryRow("SELECT COALESCE(passwd, '') FROM pg_catalog.pg_shadow AS s WHERE s.usename = $1", d.Id()).Scan(&rolePassword)
	switch {
	case err == sql.ErrNoRows:
		// They don't have a password
		return "", nil
	case err != nil:
		return "", fmt.Errorf("error reading role: %w", err)
	}
	// If the password isn't already in md5 format, but hashing the input
	// matches the password in the database for the user, they are the same
	if statePassword != "" && !strings.HasPrefix(statePassword, "md5") && !strings.HasPrefix(statePassword, "SCRAM-SHA-256") {
		if strings.HasPrefix(rolePassword, "md5") {
			hasher := md5.New()
			if _, err := hasher.Write([]byte(statePassword + d.Id())); err != nil {
				return "", err
			}
			hashedPassword := "md5" + hex.EncodeToString(hasher.Sum(nil))

			if hashedPassword == rolePassword {
				// The passwords are actually the same
				// make Terraform think they are the same
				return statePassword, nil
			}
		}
		if strings.HasPrefix(rolePassword, "SCRAM-SHA-256") {
			return statePassword, nil
			// TODO : implement scram-sha-256 challenge request to the server
		}
	}
	return rolePassword, nil
}

func resourcePostgreSQLRoleUpdate(db *DBConnection, d *schema.ResourceData) error {
	// If the update fails, keep the previous state. Otherwise the SDK saves
	// the new config, and attributes Read can't refresh (password) would look
	// applied when the transaction was rolled back.
	d.Partial(true)

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	oldName, _ := d.GetChange(roleNameAttr)
	if err := pgLockRole(txn, oldName.(string)); err != nil {
		return err
	}

	if err := setRoleName(txn, d); err != nil {
		return err
	}

	if err := setRolePassword(txn, d); err != nil {
		return err
	}

	if err := setRoleBypassRLS(db, txn, d); err != nil {
		return err
	}

	if err := setRoleConnLimit(txn, d); err != nil {
		return err
	}

	if err := setRoleCreateDB(txn, d); err != nil {
		return err
	}

	if err := setRoleCreateRole(txn, d); err != nil {
		return err
	}

	if err := setRoleInherit(txn, d); err != nil {
		return err
	}

	if err := setRoleLogin(txn, d); err != nil {
		return err
	}

	if err := setRoleReplication(txn, d); err != nil {
		return err
	}

	if err := setRoleSuperuser(txn, d); err != nil {
		return err
	}

	if err := setRoleValidUntil(txn, d); err != nil {
		return err
	}

	// applying roles: revoke the removed ones / grant the added ones.
	// Memberships kept in roles are left alone, so they keep their ADMIN
	// OPTION, and memberships granted by postgresql_grant_role since the last
	// refresh are not revoked.
	if d.HasChange(roleRolesAttr) {
		if err = updateRoleRoles(txn, d); err != nil {
			return err
		}
	}

	if err = alterSearchPath(txn, d); err != nil {
		return err
	}

	for _, attr := range roleTimeoutAttrs {
		if err = setRoleTimeout(txn, d, attr); err != nil {
			return err
		}
	}

	if err = setAssumeRole(txn, d); err != nil {
		return err
	}

	if err = txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}
	d.Partial(false)

	// Only after the commit: Read looks the role up by the ID, and in
	// partial mode a failed update still saves the ID.
	d.SetId(d.Get(roleNameAttr).(string))

	return resourcePostgreSQLRoleReadImpl(db, d)
}

func setRoleName(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleNameAttr) {
		return nil
	}

	oraw, nraw := d.GetChange(roleNameAttr)
	o := oraw.(string)
	n := nraw.(string)
	if n == "" {
		return errors.New("error setting role name to an empty string")
	}

	sql := fmt.Sprintf("ALTER ROLE %s RENAME TO %s", pq.QuoteIdentifier(o), pq.QuoteIdentifier(n))
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role NAME: %w", err)
	}

	return nil
}

func setRolePassword(txn *sql.Tx, d *schema.ResourceData) error {

	// Early exit if password WO and version are set, and version has not changed
	if _, ok := getWO(d, rolePasswordWOAttr); ok {
		if !d.HasChange(rolePasswordWOVersionAttr) {
			return nil
		}
	} else {
		// Only for regular password attribute: exit if neither password nor role name changed
		if !d.HasChange(rolePasswordAttr) && !d.HasChange(roleNameAttr) {
			return nil
		}
	}

	roleName := d.Get(roleNameAttr).(string)

	var password string
	if v, ok := getWO(d, rolePasswordWOAttr); ok {
		password = v // use the value from password_wo and reset the password state.
		d.Set(rolePasswordAttr, "")
	} else if v, ok := d.GetOk(rolePasswordAttr); ok && v.(string) != "" {
		password = v.(string) // use the clear-text password
	} else {
		// Nothing to set
		return nil
	}

	sql := fmt.Sprintf("ALTER ROLE %s %s", pq.QuoteIdentifier(roleName), rolePasswordClause(password))
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role password: %w", err)
	}

	return nil
}

// rolePasswordClause returns the PASSWORD clause of CREATE/ALTER ROLE for a
// clear-text password. pq.QuoteLiteral produces an escape string literal
// (E'...') when the password contains a backslash, so the value is stored as
// configured whatever standard_conforming_strings is set to.
func rolePasswordClause(password string) string {
	return "PASSWORD " + pq.QuoteLiteral(password)
}

func setRoleBypassRLS(db *DBConnection, txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleBypassRLSAttr) {
		return nil
	}

	if !db.featureSupported(featureRLS) {
		return fmt.Errorf("PostgreSQL client is talking with a server (%q) that does not support PostgreSQL Row-Level Security", db.version.String())
	}

	bypassRLS := d.Get(roleBypassRLSAttr).(bool)
	tok := "NOBYPASSRLS"
	if bypassRLS {
		tok = "BYPASSRLS"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role BYPASSRLS: %w", err)
	}

	return nil
}

func setRoleConnLimit(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleConnLimitAttr) {
		return nil
	}

	connLimit := d.Get(roleConnLimitAttr).(int)
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", pq.QuoteIdentifier(roleName), connLimit)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role CONNECTION LIMIT: %w", err)
	}

	return nil
}

func setRoleCreateDB(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleCreateDBAttr) {
		return nil
	}

	createDB := d.Get(roleCreateDBAttr).(bool)
	tok := "NOCREATEDB"
	if createDB {
		tok = "CREATEDB"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role CREATEDB: %w", err)
	}

	return nil
}

func setRoleCreateRole(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleCreateRoleAttr) {
		return nil
	}

	createRole := d.Get(roleCreateRoleAttr).(bool)
	tok := "NOCREATEROLE"
	if createRole {
		tok = "CREATEROLE"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role CREATEROLE: %w", err)
	}

	return nil
}

func setRoleInherit(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleInheritAttr) {
		return nil
	}

	inherit := d.Get(roleInheritAttr).(bool)
	tok := "NOINHERIT"
	if inherit {
		tok = "INHERIT"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role INHERIT: %w", err)
	}

	return nil
}

func setRoleLogin(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleLoginAttr) {
		return nil
	}

	login := d.Get(roleLoginAttr).(bool)
	tok := "NOLOGIN"
	if login {
		tok = "LOGIN"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role LOGIN: %w", err)
	}

	return nil
}

func setRoleReplication(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleReplicationAttr) {
		return nil
	}

	replication := d.Get(roleReplicationAttr).(bool)
	tok := "NOREPLICATION"
	if replication {
		tok = "REPLICATION"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role REPLICATION: %w", err)
	}

	return nil
}

func setRoleSuperuser(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleSuperuserAttr) {
		return nil
	}

	superuser := d.Get(roleSuperuserAttr).(bool)
	tok := "NOSUPERUSER"
	if superuser {
		tok = "SUPERUSER"
	}
	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s WITH %s", pq.QuoteIdentifier(roleName), tok)
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role SUPERUSER: %w", err)
	}

	return nil
}

func setRoleValidUntil(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleValidUntilAttr) {
		return nil
	}

	validUntil := d.Get(roleValidUntilAttr).(string)
	if validUntil == "" {
		return nil
	} else if strings.ToLower(validUntil) == "infinity" {
		validUntil = "infinity"
	}

	roleName := d.Get(roleNameAttr).(string)
	sql := fmt.Sprintf("ALTER ROLE %s VALID UNTIL %s", pq.QuoteIdentifier(roleName), pq.QuoteLiteral(validUntil))
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("error updating role VALID UNTIL: %w", err)
	}

	return nil
}

// roleRolesStatements returns the REVOKE statements for the roles in old but
// not in new that the role is currently a member of, then the GRANT
// statements for the roles in new but not in old, each sorted by role name.
//
// Revokes are limited to current memberships because a removed role may
// already be gone: when its postgresql_role is destroyed in the same apply,
// DROP ROLE has removed the membership with it and REVOKE would fail.
func roleRolesStatements(role string, old, new *schema.Set, current map[string]bool) []string {
	var stmts []string
	for _, r := range sortedSetStrings(old.Difference(new)) {
		if current[r] {
			stmts = append(stmts, fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(r), pq.QuoteIdentifier(role)))
		}
	}
	for _, r := range sortedSetStrings(new.Difference(old)) {
		stmts = append(stmts, fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(r), pq.QuoteIdentifier(role)))
	}
	return stmts
}

func sortedSetStrings(s *schema.Set) []string {
	out := make([]string, 0, s.Len())
	for _, v := range s.List() {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

// currentRoleRoles returns the roles the role is currently a member of.
func currentRoleRoles(txn *sql.Tx, role string) (map[string]bool, error) {
	rows, err := txn.Query(`SELECT pg_get_userbyid(roleid)
		FROM pg_catalog.pg_auth_members members
		JOIN pg_catalog.pg_roles ON members.member = pg_roles.oid
		WHERE rolname = $1`, role)
	if err != nil {
		return nil, fmt.Errorf("could not get roles list for role %s: %w", role, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	current := map[string]bool{}
	for rows.Next() {
		var grantedRole string
		if err := rows.Scan(&grantedRole); err != nil {
			return nil, fmt.Errorf("could not scan role name for role %s: %w", role, err)
		}
		current[grantedRole] = true
	}
	return current, rows.Err()
}

// updateRoleRoles applies the change of roles (from the prior state to the
// plan) to the role's memberships.
func updateRoleRoles(txn *sql.Tx, d *schema.ResourceData) error {
	role := d.Get(roleNameAttr).(string)
	oldRoles, newRoles := d.GetChange(roleRolesAttr)

	current, err := currentRoleRoles(txn, role)
	if err != nil {
		return err
	}

	for _, stmt := range roleRolesStatements(role, oldRoles.(*schema.Set), newRoles.(*schema.Set), current) {
		if _, err := txn.Exec(stmt); err != nil {
			return fmt.Errorf("could not update roles of role %s (%s): %w", role, stmt, err)
		}
	}
	return nil
}

func grantRoles(txn *sql.Tx, d *schema.ResourceData) error {
	role := d.Get(roleNameAttr).(string)

	for _, grantingRole := range d.Get("roles").(*schema.Set).List() {
		query := fmt.Sprintf(
			"GRANT %s TO %s", pq.QuoteIdentifier(grantingRole.(string)), pq.QuoteIdentifier(role),
		)
		if _, err := txn.Exec(query); err != nil {
			return fmt.Errorf("could not grant role %s to %s: %w", grantingRole, role, err)
		}
	}
	return nil
}

func alterSearchPath(txn *sql.Tx, d *schema.ResourceData) error {
	role := d.Get(roleNameAttr).(string)
	searchPathInterface := d.Get(roleSearchPathAttr).([]any)

	var searchPathString []string
	if len(searchPathInterface) > 0 {
		searchPathString = make([]string, len(searchPathInterface))
		for i, searchPathPart := range searchPathInterface {
			if strings.Contains(searchPathPart.(string), ", ") {
				return fmt.Errorf("search_path cannot contain `, `: %v", searchPathPart)
			}
			searchPathString[i] = pq.QuoteIdentifier(searchPathPart.(string))
		}
	} else {
		searchPathString = []string{"DEFAULT"}
	}
	searchPath := strings.Join(searchPathString[:], ", ")

	query := fmt.Sprintf(
		"ALTER ROLE %s SET search_path TO %s", pq.QuoteIdentifier(role), searchPath,
	)
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not set search_path %s for %s: %w", searchPath, role, err)
	}
	return nil
}

// setRoleTimeout sets a millisecond-based setting of the role, or resets it
// when it is unset in the configuration. "0" is set, not reset: it disables
// the timeout instead of inheriting the database or server value.
func setRoleTimeout(txn *sql.Tx, d *schema.ResourceData, attr string) error {
	if !d.HasChange(attr) {
		return nil
	}

	roleName := d.Get(roleNameAttr).(string)
	value := d.Get(attr).(string)
	if value == "" {
		sql := fmt.Sprintf("ALTER ROLE %s RESET %s", pq.QuoteIdentifier(roleName), attr)
		if _, err := txn.Exec(sql); err != nil {
			return fmt.Errorf("could not reset %s for %s: %w", attr, roleName, err)
		}
		return nil
	}

	sql := fmt.Sprintf("ALTER ROLE %s SET %s TO %s", pq.QuoteIdentifier(roleName), attr, pq.QuoteLiteral(value))
	if _, err := txn.Exec(sql); err != nil {
		return fmt.Errorf("could not set %s %s for %s: %w", attr, value, roleName, err)
	}
	return nil
}

func setAssumeRole(txn *sql.Tx, d *schema.ResourceData) error {
	if !d.HasChange(roleAssumeRoleAttr) {
		return nil
	}

	roleName := d.Get(roleNameAttr).(string)
	assumeRole := d.Get(roleAssumeRoleAttr).(string)
	if assumeRole != "" {
		sql := fmt.Sprintf(
			"ALTER ROLE %s SET ROLE TO %s", pq.QuoteIdentifier(roleName), pq.QuoteIdentifier(assumeRole),
		)
		if _, err := txn.Exec(sql); err != nil {
			return fmt.Errorf("could not set role %s for %s: %w", assumeRole, roleName, err)
		}
	} else {
		sql := fmt.Sprintf(
			"ALTER ROLE %s RESET ROLE", pq.QuoteIdentifier(roleName),
		)
		if _, err := txn.Exec(sql); err != nil {
			return fmt.Errorf("could not reset role for %s: %w", roleName, err)
		}
	}
	return nil
}

func getWO(d *schema.ResourceData, attribute string) (string, bool) {
	// Special case for write-only fields
	raw, diags := d.GetRawConfigAt(cty.GetAttrPath(attribute))
	if diags.HasError() || raw.IsNull() || !raw.Type().Equals(cty.String) {
		return "", false
	}
	if raw.AsString() == "" {
		return "", false
	}
	return raw.AsString(), true // return the value
}
