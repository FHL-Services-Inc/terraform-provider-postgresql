package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lib/pq"
)

const (
	roleAuditRoleNameAttr   = "role_name"
	roleAuditLogClassesAttr = "log_classes"

	pgauditLogParameter = "pgaudit.log"
)

func resourcePostgreSQLRoleAudit() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLRoleAuditCreate),
		Read:   PGResourceFunc(resourcePostgreSQLRoleAuditRead),
		Update: PGResourceFunc(resourcePostgreSQLRoleAuditUpdate),
		Delete: PGResourceFunc(resourcePostgreSQLRoleAuditDelete),
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			roleAuditRoleNameAttr: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringIsNotEmpty,
				Description:  "The name of the existing role to set pgaudit.log for",
			},
			roleAuditLogClassesAttr: {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotEmpty,
				Description:  "Comma-separated pgaudit.log statement classes for the role, e.g. all, none, ddl or read, write",
			},
		},
	}
}

func resourcePostgreSQLRoleAuditCreate(db *DBConnection, d *schema.ResourceData) error {
	role := d.Get(roleAuditRoleNameAttr).(string)

	if err := setRoleAuditLogClasses(db, role, d.Get(roleAuditLogClassesAttr).(string)); err != nil {
		return err
	}

	d.SetId(role)

	return resourcePostgreSQLRoleAuditReadImpl(db, d)
}

func resourcePostgreSQLRoleAuditRead(db *DBConnection, d *schema.ResourceData) error {
	return resourcePostgreSQLRoleAuditReadImpl(db, d)
}

func resourcePostgreSQLRoleAuditReadImpl(db *DBConnection, d *schema.ResourceData) error {
	role := d.Id()

	var roleConfig pq.ByteaArray
	err := db.QueryRow("SELECT rolconfig FROM pg_catalog.pg_roles WHERE rolname = $1", role).Scan(&roleConfig)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("[WARN] PostgreSQL role (%s) not found, removing pgaudit.log setting from state", role)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading pgaudit.log for role %s: %w", role, err)
	}

	d.Set(roleAuditRoleNameAttr, role)
	d.Set(roleAuditLogClassesAttr, readRoleAuditLogClasses(roleConfig))

	return nil
}

func resourcePostgreSQLRoleAuditUpdate(db *DBConnection, d *schema.ResourceData) error {
	if d.HasChange(roleAuditLogClassesAttr) {
		if err := setRoleAuditLogClasses(db, d.Id(), d.Get(roleAuditLogClassesAttr).(string)); err != nil {
			return err
		}
	}

	return resourcePostgreSQLRoleAuditReadImpl(db, d)
}

func resourcePostgreSQLRoleAuditDelete(db *DBConnection, d *schema.ResourceData) error {
	role := d.Id()

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	exists, err := roleExists(txn, role)
	if err != nil {
		return err
	}
	if !exists {
		d.SetId("")
		return nil
	}

	if err := pgLockRole(txn, role); err != nil {
		return err
	}

	query := fmt.Sprintf("ALTER ROLE %s RESET %s", pq.QuoteIdentifier(role), pgauditLogParameter)
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not reset pgaudit.log for role %s: %w", role, err)
	}

	if err := txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}

	d.SetId("")

	return nil
}

func setRoleAuditLogClasses(db *DBConnection, role, value string) error {
	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, role); err != nil {
		return err
	}

	query := fmt.Sprintf(
		"ALTER ROLE %s SET %s TO %s",
		pq.QuoteIdentifier(role), pgauditLogParameter, pq.QuoteLiteral(value),
	)
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not set pgaudit.log for role %s: %w", role, err)
	}

	if err := txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}

	return nil
}

func readRoleAuditLogClasses(roleConfig pq.ByteaArray) string {
	prefix := pgauditLogParameter + "="
	for _, v := range roleConfig {
		if config := string(v); strings.HasPrefix(config, prefix) {
			return strings.TrimPrefix(config, prefix)
		}
	}
	return ""
}
