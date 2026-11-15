package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

const (
	userMappingUserNameAttr   = "user_name"
	userMappingServerNameAttr = "server_name"
	userMappingOptionsAttr    = "options"
)

func resourcePostgreSQLUserMapping() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLUserMappingCreate),
		Read:   PGResourceFunc(resourcePostgreSQLUserMappingRead),
		Update: PGResourceFunc(resourcePostgreSQLUserMappingUpdate),
		Delete: PGResourceFunc(resourcePostgreSQLUserMappingDelete),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLUserMappingImport,
		},

		Schema: map[string]*schema.Schema{
			userMappingUserNameAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of an existing user that is mapped to foreign server. CURRENT_ROLE, CURRENT_USER, and USER match the name of the current user. When PUBLIC is specified, a so-called public mapping is created that is used when no user-specific mapping is applicable",
			},
			userMappingServerNameAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of an existing server for which the user mapping is to be created",
			},
			userMappingOptionsAttr: {
				Type: schema.TypeMap,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
				Optional:    true,
				Description: "This clause specifies the options of the user mapping. The options typically define the actual user name and password of the mapping. Option names must be unique. The allowed option names and values are specific to the server's foreign-data wrapper",
			},
		},
	}
}

func resourcePostgreSQLUserMappingCreate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureServer) {
		return fmt.Errorf(
			"foreign server resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	username := d.Get(userMappingUserNameAttr).(string)
	serverName := d.Get(userMappingServerNameAttr).(string)

	b := bytes.NewBufferString("CREATE USER MAPPING ")
	fmt.Fprint(b, " FOR ", userMappingUserSQL(username))
	fmt.Fprint(b, " SERVER ", pq.QuoteIdentifier(serverName))

	if options, ok := d.GetOk(userMappingOptionsAttr); ok {
		fmt.Fprint(b, " OPTIONS ( ")
		cnt := 0
		len := len(options.(map[string]any))
		for k, v := range options.(map[string]any) {
			fmt.Fprint(b, " ", pq.QuoteIdentifier(k), " ", pq.QuoteLiteral(v.(string)))
			if cnt < len-1 {
				fmt.Fprint(b, ", ")
			}
			cnt++
		}
		fmt.Fprint(b, " ) ")
	}

	if _, err := db.Exec(b.String()); err != nil {
		return fmt.Errorf("could not create user mapping: %w", err)
	}

	d.SetId(generateUserMappingID(d))

	return resourcePostgreSQLUserMappingReadImpl(db, d)
}

func resourcePostgreSQLUserMappingRead(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureServer) {
		return fmt.Errorf(
			"foreign server resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	return resourcePostgreSQLUserMappingReadImpl(db, d)
}

func resourcePostgreSQLUserMappingReadImpl(db *DBConnection, d *schema.ResourceData) error {
	username := d.Get(userMappingUserNameAttr).(string)
	serverName := d.Get(userMappingServerNameAttr).(string)

	// A PUBLIC mapping has umuser = 0. The views spell its user differently
	// ('PUBLIC' in information_schema._pg_user_mappings, 'public' in
	// pg_user_mappings) and a real role may be named "PUBLIC", so match it on
	// umuser instead of the name.
	userFilter := "umuser <> 0 AND authorization_identifier = $2"
	fallbackFilter := "umuser <> 0 AND usename = $2"
	args := []any{serverName, username}
	if isPublicUserMapping(username) {
		userFilter = "umuser = 0"
		fallbackFilter = userFilter
		args = args[:1]
	}

	var userMappingOptions []string
	query := "SELECT umoptions FROM information_schema._pg_user_mappings WHERE foreign_server_name = $1 AND " + userFilter
	err := db.QueryRow(query, args...).Scan(pq.Array(&userMappingOptions))

	// pg_user_mappings shows umoptions only to the mapped role itself or to a
	// superuser; anyone else gets NULL, even the owner of the server.
	optionsMasked := false
	if err != sql.ErrNoRows && err != nil {
		// Fallback to pg_user_mappings table if information_schema._pg_user_mappings is not available
		query := "SELECT umoptions FROM pg_user_mappings WHERE srvname = $1 AND " + fallbackFilter
		err = db.QueryRow(query, args...).Scan(pq.Array(&userMappingOptions))
		optionsMasked = err == nil && userMappingOptions == nil
	}

	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL user mapping (%s) for server (%s) not found", username, serverName)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading user mapping: %w", err)
	}

	d.Set(userMappingUserNameAttr, username)
	d.Set(userMappingServerNameAttr, serverName)
	if optionsMasked {
		// The options can't be read back, so keep the ones in state instead
		// of reporting a permanent diff. Out-of-band changes to the options
		// are not detected in this case.
		log.Printf("[DEBUG] PostgreSQL user mapping (%s) for server (%s): options are not visible to the current user, keeping them from state", username, serverName)
	} else {
		mappedOptions := make(map[string]any)
		for _, v := range userMappingOptions {
			pair := strings.SplitN(v, "=", 2)
			mappedOptions[pair[0]] = pair[1]
		}
		d.Set(userMappingOptionsAttr, mappedOptions)
	}
	d.SetId(generateUserMappingID(d))

	return nil
}

func resourcePostgreSQLUserMappingDelete(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureServer) {
		return fmt.Errorf(
			"foreign server resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	username := d.Get(userMappingUserNameAttr).(string)
	serverName := d.Get(userMappingServerNameAttr).(string)

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	sql := fmt.Sprintf("DROP USER MAPPING FOR %s SERVER %s ", userMappingUserSQL(username), pq.QuoteIdentifier(serverName))
	if _, err := txn.Exec(sql); err != nil {
		return err
	}

	if err = txn.Commit(); err != nil {
		return fmt.Errorf("error deleting user mapping: %w", err)
	}

	d.SetId("")

	return nil
}

func resourcePostgreSQLUserMappingUpdate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureServer) {
		return fmt.Errorf(
			"foreign server resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	if err := setUserMappingOptionsIfChanged(db, d); err != nil {
		return err
	}

	return resourcePostgreSQLUserMappingReadImpl(db, d)
}

func setUserMappingOptionsIfChanged(db *DBConnection, d *schema.ResourceData) error {
	if !d.HasChange(userMappingOptionsAttr) {
		return nil
	}

	username := d.Get(userMappingUserNameAttr).(string)
	serverName := d.Get(userMappingServerNameAttr).(string)

	b := bytes.NewBufferString("ALTER USER MAPPING ")
	fmt.Fprintf(b, " FOR %s SERVER %s ", userMappingUserSQL(username), pq.QuoteIdentifier(serverName))

	oldOptions, newOptions := d.GetChange(userMappingOptionsAttr)
	fmt.Fprint(b, " OPTIONS ( ")
	cnt := 0
	len := len(newOptions.(map[string]any))
	toRemove := oldOptions.(map[string]any)
	for k, v := range newOptions.(map[string]any) {
		operation := "ADD"
		if oldOptions.(map[string]any)[k] != nil {
			operation = "SET"
			delete(toRemove, k)
		}
		fmt.Fprintf(b, " %s %s %s ", operation, pq.QuoteIdentifier(k), pq.QuoteLiteral(v.(string)))
		if cnt < len-1 {
			fmt.Fprint(b, ", ")
		}
		cnt++
	}

	for k := range toRemove {
		if cnt != 0 { // starting with 0 means to drop all the options. Cannot start with comma
			fmt.Fprint(b, " , ")
		}
		fmt.Fprintf(b, " DROP %s ", pq.QuoteIdentifier(k))
		cnt++
	}

	fmt.Fprint(b, " ) ")

	if _, err := db.Exec(b.String()); err != nil {
		return fmt.Errorf("error updating user mapping options: %w", err)
	}

	return nil
}

func generateUserMappingID(d *schema.ResourceData) string {
	return strings.Join([]string{
		d.Get(userMappingUserNameAttr).(string),
		d.Get(userMappingServerNameAttr).(string),
	}, ".")
}

// isPublicUserMapping reports whether username means the PUBLIC pseudo-role,
// like the unquoted keyword PUBLIC in SQL (any case).
func isPublicUserMapping(username string) bool {
	return strings.EqualFold(username, "public")
}

// userMappingUserSQL renders username for CREATE/ALTER/DROP USER MAPPING FOR.
// PUBLIC must stay unquoted: "PUBLIC" (quoted) would name a regular role.
func userMappingUserSQL(username string) string {
	if isPublicUserMapping(username) {
		return "PUBLIC"
	}
	return pq.QuoteIdentifier(username)
}

// userMappingImportFormat describes the import ID, which is the resource ID.
const userMappingImportFormat = "<user_name>.<server_name>"

// userMappingCatalogEntry is a user mapping as pg_user_mappings shows it.
type userMappingCatalogEntry struct {
	public       bool // umuser = 0
	user, server string
}

// userMappingLookup returns the user mappings of the given servers.
type userMappingLookup func(servers []string) ([]userMappingCatalogEntry, error)

// parseUserMappingImportID splits an import ID "<user_name>.<server_name>"
// into its parts. Role and server names may contain dots, so the ID is split
// at every dot and the split that names an existing user mapping wins; an ID
// that matches no mapping, or more than one, is rejected. user_name keeps the
// spelling of the ID, so "public" and "PUBLIC" both import the PUBLIC mapping
// as written in the configuration.
func parseUserMappingImportID(id string, lookup userMappingLookup) (string, string, error) {
	fail := func(err error) (string, string, error) {
		return "", "", importParseError("postgresql_user_mapping", id, userMappingImportFormat, err)
	}

	type split struct{ user, server string }
	var splits []split
	var servers []string
	for i := range len(id) {
		if id[i] == '.' {
			splits = append(splits, split{user: id[:i], server: id[i+1:]})
			servers = append(servers, id[i+1:])
		}
	}
	if len(splits) == 0 {
		return fail(errors.New("no user mapping matches the ID"))
	}

	mappings, err := lookup(servers)
	if err != nil {
		return fail(fmt.Errorf("could not look up user mappings: %w", err))
	}

	var candidates []split
	var descr []string
	for _, s := range splits {
		for _, m := range mappings {
			if m.server != s.server {
				continue
			}
			if isPublicUserMapping(s.user) && m.public || !isPublicUserMapping(s.user) && !m.public && m.user == s.user {
				candidates = append(candidates, s)
				descr = append(descr, fmt.Sprintf("user_name=%s server_name=%s", s.user, s.server))
				break
			}
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0].user, candidates[0].server, nil
	case 0:
		return fail(errors.New("no user mapping matches the ID"))
	default:
		return fail(ambiguousImportError(descr))
	}
}

// lookupUserMappings reads the user mappings of servers from
// pg_user_mappings, which every role can read (only the options are masked).
func lookupUserMappings(db QueryAble) userMappingLookup {
	return func(servers []string) ([]userMappingCatalogEntry, error) {
		rows, err := db.Query(
			"SELECT umuser = 0, usename, srvname FROM pg_catalog.pg_user_mappings WHERE srvname = ANY($1)",
			pq.Array(servers),
		)
		if err != nil {
			return nil, err
		}
		defer closeRows(rows)

		var out []userMappingCatalogEntry
		for rows.Next() {
			var m userMappingCatalogEntry
			if err := rows.Scan(&m.public, &m.user, &m.server); err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
}

func resourcePostgreSQLUserMappingImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	db, err := meta.(*Client).Connect()
	if err != nil {
		return nil, err
	}

	username, serverName, err := parseUserMappingImportID(d.Id(), lookupUserMappings(db))
	if err != nil {
		return nil, err
	}
	if err := d.Set(userMappingUserNameAttr, username); err != nil {
		return nil, fmt.Errorf("could not set %s: %w", userMappingUserNameAttr, err)
	}
	if err := d.Set(userMappingServerNameAttr, serverName); err != nil {
		return nil, fmt.Errorf("could not set %s: %w", userMappingServerNameAttr, err)
	}

	return []*schema.ResourceData{d}, nil
}
