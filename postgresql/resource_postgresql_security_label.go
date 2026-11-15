package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

const (
	securityLabelObjectNameAttr = "object_name"
	securityLabelObjectTypeAttr = "object_type"
	securityLabelProviderAttr   = "label_provider"
	securityLabelLabelAttr      = "label"
)

func resourcePostgreSQLSecurityLabel() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLSecurityLabelCreate),
		Read:   PGResourceFunc(resourcePostgreSQLSecurityLabelRead),
		Update: PGResourceFunc(resourcePostgreSQLSecurityLabelUpdate),
		Delete: PGResourceFunc(resourcePostgreSQLSecurityLabelDelete),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLSecurityLabelImport,
		},

		Schema: map[string]*schema.Schema{
			securityLabelObjectNameAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of the existing object to apply the security label to",
			},
			securityLabelObjectTypeAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The type of the existing object to apply the security label to",
			},
			securityLabelProviderAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The provider to apply the security label for",
			},
			securityLabelLabelAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    false,
				Description: "The label to be applied",
			},
		},
	}
}

func resourcePostgreSQLSecurityLabelCreate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Create")
	label := d.Get(securityLabelLabelAttr).(string)
	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, pq.QuoteLiteral(label)); err != nil {
		return err
	}

	d.SetId(generateSecurityLabelID(d))

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

func resourcePostgreSQLSecurityLabelUpdateImpl(db *DBConnection, d *schema.ResourceData, label string) error {
	b := bytes.NewBufferString("SECURITY LABEL ")

	objectType := d.Get(securityLabelObjectTypeAttr).(string)
	objectName := d.Get(securityLabelObjectNameAttr).(string)
	provider := d.Get(securityLabelProviderAttr).(string)
	fmt.Fprint(b, " FOR ", pq.QuoteIdentifier(provider))
	fmt.Fprint(b, " ON ", objectType, pq.QuoteIdentifier(objectName))
	fmt.Fprint(b, " IS ", label)

	if _, err := db.Exec(b.String()); err != nil {
		log.Printf("[WARN] PostgreSQL security label Create failed %s", err)
		return fmt.Errorf("could not create security label: %w", err)
	}
	return nil
}

func resourcePostgreSQLSecurityLabelRead(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Read")

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

// securityLabelReadFilter selects the label of an object in pg_seclabels. Its
// arguments are the object type, quoteIdentifier(object name) and
// quoteIdentifier(provider). Read and the importer share it so that an ID the
// importer accepts is one Read finds.
const securityLabelReadFilter = "objtype = $1 and objname = $2 and provider = $3"

func resourcePostgreSQLSecurityLabelReadImpl(db *DBConnection, d *schema.ResourceData) error {
	objectType := d.Get(securityLabelObjectTypeAttr).(string)
	objectName := d.Get(securityLabelObjectNameAttr).(string)
	provider := d.Get(securityLabelProviderAttr).(string)

	query := "SELECT objtype, provider, objname, label FROM pg_seclabels WHERE " + securityLabelReadFilter
	row := db.QueryRow(query, objectType, quoteIdentifier(objectName), quoteIdentifier(provider))

	var label, newObjectName, newProvider string
	err := row.Scan(&objectType, &newProvider, &newObjectName, &label)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL security label for (%s '%s') with provider %s not found", objectType, objectName, provider)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading security label: %w", err)
	}

	if quoteIdentifier(objectName) != newObjectName || quoteIdentifier(provider) != newProvider {
		// In reality, this should never happen, but if it does, we want to make sure that the state is in sync with the remote system
		// This will trigger a TF error saying that the provider has a bug if it ever happens
		objectName = newObjectName
		provider = newProvider
	}
	d.Set(securityLabelObjectTypeAttr, objectType)
	d.Set(securityLabelObjectNameAttr, objectName)
	d.Set(securityLabelProviderAttr, provider)
	d.Set(securityLabelLabelAttr, label)
	d.SetId(generateSecurityLabelID(d))

	return nil
}

func resourcePostgreSQLSecurityLabelDelete(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Delete")

	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, "NULL"); err != nil {
		return err
	}

	d.SetId("")

	return nil
}

func resourcePostgreSQLSecurityLabelUpdate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureServer) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Update")

	label := d.Get(securityLabelLabelAttr).(string)
	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, pq.QuoteLiteral(label)); err != nil {
		return err
	}

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

func generateSecurityLabelID(d *schema.ResourceData) string {
	return strings.Join([]string{
		d.Get(securityLabelProviderAttr).(string),
		d.Get(securityLabelObjectTypeAttr).(string),
		d.Get(securityLabelObjectNameAttr).(string),
	}, ".")
}

func quoteIdentifier(s string) string {
	var result = s
	re := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	if !re.MatchString(s) || s != strings.ToLower(s) {
		result = pq.QuoteIdentifier(s)
	}
	return result
}

// securityLabelImportFormat describes the import ID, which is the resource ID.
const securityLabelImportFormat = "<label_provider>.<object_type>.<object_name>"

// securityLabelObjectTypes are the object types of SECURITY LABEL ON, in the
// lower case pg_seclabels.objtype and Read use.
var securityLabelObjectTypes = map[string]bool{
	"table": true, "column": true, "aggregate": true, "database": true,
	"domain": true, "event trigger": true, "foreign table": true,
	"function": true, "large object": true, "materialized view": true,
	"language": true, "procedure": true, "publication": true, "role": true,
	"routine": true, "schema": true, "sequence": true, "subscription": true,
	"tablespace": true, "type": true, "view": true,
}

// securityLabelLookup reports whether a security label exists for the given
// object type (lower case), object name and provider.
type securityLabelLookup func(objType, objName, provider string) (bool, error)

// parseSecurityLabelImportID splits an import ID
// "<label_provider>.<object_type>.<object_name>" into its parts. Provider and
// object names may contain dots, so every pair of dots that encloses a known
// object type (in any case) is a candidate split, and the split that names an
// existing security label wins; an ID that matches no label, or more than one,
// is rejected. The object type is returned in lower case, as Read stores it.
func parseSecurityLabelImportID(id string, lookup securityLabelLookup) (provider, objType, objName string, err error) {
	fail := func(err error) (string, string, string, error) {
		return "", "", "", importParseError("postgresql_security_label", id, securityLabelImportFormat, err)
	}

	type split struct{ provider, objType, objName string }
	var candidates []split
	var descr []string
	for i := range len(id) {
		if id[i] != '.' {
			continue
		}
		for j := i + 1; j < len(id); j++ {
			if id[j] != '.' {
				continue
			}
			t := strings.ToLower(id[i+1 : j])
			if !securityLabelObjectTypes[t] {
				continue
			}
			s := split{provider: id[:i], objType: t, objName: id[j+1:]}
			exists, err := lookup(s.objType, s.objName, s.provider)
			if err != nil {
				return fail(fmt.Errorf("could not look up security labels: %w", err))
			}
			if exists {
				candidates = append(candidates, s)
				descr = append(descr, fmt.Sprintf("label_provider=%s object_type=%s object_name=%s", s.provider, s.objType, s.objName))
			}
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0].provider, candidates[0].objType, candidates[0].objName, nil
	case 0:
		return fail(errors.New("no security label matches the ID"))
	default:
		return fail(ambiguousImportError(descr))
	}
}

// lookupSecurityLabel checks pg_seclabels with the filter and quoting Read uses.
func lookupSecurityLabel(db QueryAble) securityLabelLookup {
	return func(objType, objName, provider string) (bool, error) {
		var exists bool
		err := db.QueryRow(
			"SELECT TRUE FROM pg_seclabels WHERE "+securityLabelReadFilter,
			objType, quoteIdentifier(objName), quoteIdentifier(provider),
		).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}
}

func resourcePostgreSQLSecurityLabelImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	db, err := meta.(*Client).Connect()
	if err != nil {
		return nil, err
	}
	if !db.featureSupported(featureSecurityLabel) {
		return nil, fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	provider, objType, objName, err := parseSecurityLabelImportID(d.Id(), lookupSecurityLabel(db))
	if err != nil {
		return nil, err
	}
	for attr, v := range map[string]string{
		securityLabelProviderAttr:   provider,
		securityLabelObjectTypeAttr: objType,
		securityLabelObjectNameAttr: objName,
	} {
		if err := d.Set(attr, v); err != nil {
			return nil, fmt.Errorf("could not set %s: %w", attr, err)
		}
	}

	return []*schema.ResourceData{d}, nil
}
