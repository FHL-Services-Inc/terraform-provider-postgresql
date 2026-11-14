---
layout: "postgresql"
page_title: "Provider: PostgreSQL"
sidebar_current: "docs-postgresql-index"
description: |-
  A provider for PostgreSQL Server.
---

# PostgreSQL Provider

The PostgreSQL provider gives the ability to deploy and configure resources in a PostgreSQL server.

Use the navigation to the left to read about the available resources.

For large configurations, see the [performance tuning
guide](guides/performance-tuning.html): connection reuse
(`max_idle_connections`) and `binary_parameters` can make plans and applies
with many resources much faster, especially against a remote server.

## Usage

```hcl
provider "postgresql" {
  host            = "postgres_server_ip"
  port            = 5432
  database        = "postgres"
  username        = "postgres_user"
  password        = "postgres_password"
  sslmode         = "require"
  connect_timeout = 15
}
```

An SSL client certificate can be configured using the `clientcert` sub-resource.

``` hcl
provider "postgresql" {
  host            = "postgres_server_ip"
  port            = 5432
  database        = "postgres"
  username        = "postgres_user"
  password        = "postgres_password"
  sslmode         = "require"
  clientcert {
    cert = "/path/to/public-certificate.pem"
    key  = "/path/to/private-key.pem"
  }
}
```

Configuring multiple servers can be done by specifying the alias option.

```hcl
provider "postgresql" {
  alias    = "pg1"
  host     = "postgres_server_ip1"
  username = "postgres_user1"
  password = "postgres_password1"
}

provider "postgresql" {
  alias    = "pg2"
  host     = "postgres_server_ip2"
  username = "postgres_user2"
  password = "postgres_password2"
}

resource "postgresql_database" "my_db1" {
  provider = "postgresql.pg1"
  name     = "my_db1"
}

resource "postgresql_database" "my_db2" {
  provider = "postgresql.pg2"
  name     = "my_db2"
}
```

## Injecting Credentials
There are several methods of providing credentials to the provider without hardcoding them.

### Environment Variables
Provider settings can be specified via environment variables as follows:

```shell
export PGHOST=localhost
export PGPORT=5432
export PGUSER=postgres
export PGPASSWORD=postgres
```

### Terraform Variables
Input variables can be used in provider configuration. These variables can be initialised in your Terraform code, via a [variable file](https://developer.hashicorp.com/terraform/language/values/variables#variable-definitions-tfvars-files), via [`TF_VAR_` environment variables](https://developer.hashicorp.com/terraform/language/values/variables#environment-variables) or any other method that Terraform allows.

For example:
```hcl
variable "host" {
  default = "localhost"
}

variable "password" {
  default = "adm"
}

variable "port" {
  default = 55432
}

provider "postgresql" {
  host     = var.host
  port     = var.port
  password = var.password
  sslmode  = "disable"
}

resource postgresql_database "test" {
  name = "test"
}
```

You could set the `host` variable by setting the environment variable `TF_VAR_host`.

### Data Sources and Resources
Credentials can be referenced via Terraform data sources, or resource attributes. This is useful for getting values from a secrets store such as AWS Secrets Manager.

Resource attributes may only be referenced in provider config where the value is available in the resource definition; per [Terraform docs](https://developer.hashicorp.com/terraform/language/providers/configuration#provider-configuration-1):

> you can safely reference input variables, but not attributes exported by resources (with an exception for resource arguments that are specified directly in the configuration).

For example:

```hcl
data "aws_secretsmanager_secret" "postgres_password" {
  name = "postgres_password"
}
data "aws_secretsmanager_secret_version" "postgres_password" {
  secret_id = data.aws_secretsmanager_secret.postgres_password.id
}

provider "postgresql" {
   [...]
   password = jsondecode(data.aws_secretsmanager_secret_version.postgres_password.secret_string)["password"]
}
```

## Argument Reference

The following arguments are supported:

* `scheme` - (Optional) The driver to use. Valid values are:
  * `postgres`: Default value, use [`lib/pq`][libpq]
  * `awspostgres`: Use [GoCloud](#gocloud) for AWS
  * `gcppostgres`: Use [GoCloud](#gocloud) for GCP
* `host` - (Required) The address for the postgresql server connection, see [GoCloud](#gocloud) for specific format.
* `port` - (Optional) The port for the postgresql server connection. The default is `5432`.
* `database` - (Optional) Database to connect to. The default is `postgres`.
* `username` - (Required) Username for the server connection.
* `password` - (Optional) Password for the server connection.
* `database_username` - (Optional) Username of the user in the database if different than connection username (See [user name maps](https://www.postgresql.org/docs/current/auth-username-maps.html)).
* `superuser` - (Optional) Should be set to `false` if the user to connect is not a PostgreSQL superuser (as is the case in AWS RDS or GCP SQL).
  In this case, some features might be disabled (e.g.: Refreshing state password from database).
* `sslmode` - (Optional) Set the priority for an SSL connection to the server.
  Valid values for `sslmode` are (note: `prefer` is not supported by Go's
  [`lib/pq`][libpq]):
    * disable - No SSL
    * require - Always SSL (the default, also skip verification)
    * verify-ca - Always SSL (verify that the certificate presented by the server was signed by a trusted CA)
    * verify-full - Always SSL (verify that the certification presented by the server was signed by a trusted CA and the server host name matches the one in the certificate)
  Additional information on the options and their implications can be seen
  [in the `libpq(3)` SSL guide](http://www.postgresql.org/docs/current/static/libpq-ssl.html#LIBPQ-SSL-PROTECTION).
* `clientcert` - (Optional) - Configure the SSL client certificate.
  * `cert` - (Required) - The SSL client certificate file path. The file must contain PEM encoded data.
  * `key` - (Required) - The SSL client certificate private key file path. The file must contain PEM encoded data.
  * `sslinline` - (Optional) - If set to `true`, arguments accept inline ssl cert and key rather than a filename. Defaults to `false`.
* `sslrootcert` - (Optional) - The SSL server root certificate file path. The file must contain PEM encoded data.
* `connect_timeout` - (Optional) Maximum wait for a new connection to be
  established, in seconds. The default is `180`. Zero (or `-1`) means wait
  indefinitely. Only used with the `postgres` scheme; the GoCloud schemes
  ignore it. Can also be set with the `PGCONNECT_TIMEOUT` environment variable.
* `max_conn_retries` - (Optional) Maximum number of retries when opening a
  connection fails. Authentication errors and unknown databases are not
  retried. Zero means no retries. The default is `0`.
* `connection_retry_timeout_seconds` - (Optional) Maximum total wait, in
  seconds, across all connection retries. The default is `5`.
* `max_connections` - (Optional) Maximum number of open connections in each
  connection pool. The default is `20`. Zero means unlimited.

  The provider keeps one pool per database it connects to: the `database`
  above (used for roles, databases and other cluster-wide objects) plus every
  database named in the `database` argument of a resource. The limit applies
  to each pool, not to the server as a whole, so a configuration that manages
  objects in 4 databases can open up to 5 × `max_connections` connections. The
  number of connections in use at the same time is also bounded by Terraform's
  `-parallelism` (default 10), so there is rarely a reason to set
  `max_connections` below `-parallelism`; doing so makes resources wait for a
  free connection.
* `max_idle_connections` - (Optional) Maximum number of idle connections each
  pool keeps open for reuse. The default is `0`: every connection is closed as
  soon as it is returned to the pool, so almost every statement outside a
  transaction opens a new connection (TCP, TLS and authentication handshake).
  Setting it above zero reuses connections instead, which makes plans and
  applies with many resources considerably faster, especially when the server
  is not close to where Terraform runs. See the
  [performance tuning guide](guides/performance-tuning.html) for numbers and
  recommended values. Can also be set with the `PGMAXIDLECONNECTIONS`
  environment variable.

  Things to know when it is enabled:

  * The limit applies per pool, so per database, not in total. Idle
    connections are closed after 30 seconds without use, and when they reach
    `conn_max_lifetime_seconds`. The value is capped to `max_connections` when
    the latter is non-zero.
  * The idle connections are real sessions on the server: they count against
    the server's `max_connections` setting while Terraform runs.
  * `postgresql_database` closes the provider's own pooled connections to a
    database before dropping or renaming it, and before using it as the
    `template` of another database.
  * A `DROP DATABASE`, `ALTER DATABASE ... RENAME` or
    `CREATE DATABASE ... TEMPLATE` run outside of the provider while Terraform
    runs can fail with "database is being accessed by other users" because
    of these idle connections. They stay open until 30 seconds after the
    provider last used them, or until Terraform exits. Terminate them first
    (`pg_terminate_backend`) or use `DROP DATABASE ... WITH (FORCE)` on
    PostgreSQL 13+.
  * Terraform runs every `provider` block, aliases included, in its own
    plugin process, so every block has its own pools even when two blocks
    connect to the same server, user and database. Their connections add up.
    `postgresql_database` only closes the pools of its own provider block
    before a drop, rename or template copy. Idle connections of another block
    to that database are terminated by the `pg_terminate_backend` that runs
    before `DROP DATABASE`, but they can make a rename or a template copy
    fail.
* `conn_max_lifetime_seconds` - (Optional) Maximum lifetime of a connection,
  in seconds. Older connections are closed instead of being reused (a running
  statement is not interrupted). The default is `0`. Zero means unlimited.
* `binary_parameters` - (Optional) If set to `true`, adds `binary_parameters=yes`
  to the connection string. The driver ([`lib/pq`][libpq]) then sends each
  statement that has bind parameters (almost every query the provider runs) in
  a single round-trip instead of two (a separate prepare step followed by the
  execution). The number of statements and connections does not change; only
  the round-trips per statement do, so it mostly helps when the database is far
  from where Terraform runs. Despite its name, only `[]byte` parameters are sent
  in binary format, which the provider does not use; all other parameters and
  all results are still sent as text. Works with every `scheme`. Can also be
  set with the `PGBINARYPARAMETERS` environment variable. The default is
  `false`.
* `expected_version` - (Optional) The PostgreSQL version of the server, as a
  version number such as `16`, `15.4` or `9.6.24`. By default (`9.0.0`) the
  provider detects the version with `SELECT version()` on each new pool and
  enables the features the server supports. When it is set to anything else,
  the provider skips the detection and uses the given version instead, so it
  must not be newer than the real server: features of a newer version would be
  used against an older server. Set it to talk to a server whose version
  string cannot be parsed. Default: `9.0.0`.
* `catalog_snapshot` - (Optional) Opt-in, off by default. If set to `true`,
  `postgresql_grant` (all object types but `column`, `foreign_data_wrapper` and
  `foreign_server`) and
  `postgresql_default_privileges` are read from one snapshot of the catalogs per
  database (`pg_roles`, `pg_namespace`, `pg_class`, `pg_proc`, `pg_default_acl`,
  `pg_database`), taken by one statement the first time a resource of that database
  is read and shared by every other read of that database, instead of one catalog
  query per resource. The snapshot is dropped after every write this provider makes
  to that database (roles, memberships and databases drop every snapshot), after
  `catalog_snapshot_ttl_seconds`, and the reads that follow a create or update
  always query the database. Changes made out-of-band while a Terraform run is in
  progress are seen by the next run. Can also be set with the `PGCATALOGSNAPSHOT`
  environment variable. The default is `false`.
* `catalog_snapshot_ttl_seconds` - (Optional) Maximum age, in seconds, of a catalog
  snapshot before it is taken again; also how long reads go direct after the
  snapshot of a database could not be taken. Can also be set with the
  `PGCATALOGSNAPSHOTTTL` environment variable. The default is `30`.
* `aws_rds_iam_auth` - (Optional) If set to `true`, call the AWS RDS API to grab a temporary password, using AWS Credentials
  from the environment (or the given profile, see `aws_rds_iam_profile`). With `scheme = "postgres"` (the default) the
  token is cached and renewed before it expires (RDS tokens are valid for 15 minutes), and every new connection uses
  a current one, so long-running applies keep working. With the GoCloud schemes the token obtained at provider
  configuration is used for the whole run.
* `aws_rds_iam_profile` - (Optional) The AWS IAM Profile to use while using AWS RDS IAM Auth.
* `aws_rds_iam_region` - (Optional) The AWS region to use while using AWS RDS IAM Auth.
* `aws_rds_iam_provider_role_arn` - (Optional) AWS IAM role to assume while using AWS RDS IAM Auth.
* `azure_identity_auth` - (Optional) If set to `true`, call the Azure OAuth token endpoint for temporary token. As with
  `aws_rds_iam_auth`, the token is renewed before it expires and every new connection uses a current one.
* `azure_tenant_id` - (Optional) (Required if `azure_identity_auth` is `true`) Azure tenant ID [read more](https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/data-sources/client_config.html)

## GoCloud

By default, the provider uses the [lib/pq][libpq] library to directly connect to PostgreSQL host instance. For connections to AWS/GCP hosted instances, the provider can connect through the [GoCloud](https://gocloud.dev/howto/sql/) library. GoCloud simplifies connecting to AWS/GCP hosted databases, managing any proxy or custom authentication details.

### AWS

To enable GoCloud based connections to AWS RDS instances, set `scheme` to `awspostgres` and `host` to the RDS database's endpoint value.
(e.g.: `instance.xxxxxx.region.rds.amazonaws.com`)

```hcl
provider "postgresql" {
  scheme   = "awspostgres"
  host     = "test-instance.cvvrsv6scpgd.eu-central-1.rds.amazonaws.com"
  username = "postgres"
  port     = 5432
  password = "test1234"

  superuser = false
}
```

### GCP

To enable GoCloud for GCP SQL, set `scheme` to `gcppostgres` and `host` to the connection name of the instance in following format: `project/region/instance` (or `project:region:instance`).

For GCP, GoCloud also requires the `GOOGLE_APPLICATION_CREDENTIALS` environment variable to be set to the service account credentials file.
These credentials can be created here: https://console.cloud.google.com/iam-admin/serviceaccounts

In addition, the provider supports service account impersonation with the `gcp_iam_impersonate_service_account` option. You must ensure:

- The IAM database user has sufficient permissions to connect to the database, e.g., `roles/cloudsql.instanceUser`
- The principal (IAM user or IAM service account) behind the `GOOGLE_APPLICATION_CREDENTIALS` has sufficient permissions to impersonate the provided service account. Learn more from [roles for service account authentication](https://cloud.google.com/iam/docs/service-account-permissions).

```hcl
provider "postgresql" {
  scheme   = "gcppostgres"
  host     = "test-project/europe-west3/test-instance"
  port     = 5432

  username                            = "service_account_id@$project_id.iam"
  gcp_iam_impersonate_service_account = "service_account_id@$project_id.iam.gserviceaccount.com"

  superuser = false
}
```

See also: 

- https://cloud.google.com/docs/authentication/production
- https://cloud.google.com/sql/docs/postgres/iam-logins

---
**Note**

[Cloud SQL API](https://console.developers.google.com/apis/api/sqladmin.googleapis.com/overview) needs to be enabled for GoCloud to connect to your instance.

---

```hcl
provider "postgresql" {
  scheme   = "gcppostgres"
  host     = "test-project/europe-west3/test-instance"
  username = "postgres"
  port     = 5432
  password = "test1234"

  superuser = false
}
```

Example with GCP resources:

```hcl
resource "google_sql_database_instance" "test" {
  project          = "test-project"
  name             = "test-instance"
  database_version = "POSTGRES_13"
  region           = "europe-west3"

  settings {
    tier            = "db-f1-micro"
  }
}

resource "google_sql_user" "postgres" {
  project  = "test-project"
  name     = "postgres"
  instance = google_sql_database_instance.test.name
  password = "xxxxxxxx"
}


provider "postgresql" {
  scheme   = "gcppostgres"
  host     = google_sql_database_instance.test.connection_name
  username = google_sql_user.postgres.name
  password = google_sql_user.postgres.password
}

resource postgresql_database "test_db" {
  name = "test_db"
}
```

### Azure

To enable [passwordless authentication](https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/how-to-configure-sign-in-azure-ad-authentication) with MS Azure set `azure_identity_auth` to `true` and provide `azure_tenant_id`

```hcl
data "azurerm_client_config" "current" {
}

# https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/resources/postgresql_flexible_server
resource "azurerm_postgresql_flexible_server" "pgsql" {
  # ...
  authentication {
    active_directory_auth_enabled = true
    password_auth_enabled         = false
    tenant_id                     = data.azurerm_client_config.current.tenant_id
  }
}

# https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/resources/postgresql_flexible_server_active_directory_administrator
resource "azurerm_postgresql_flexible_server_active_directory_administrator" "administrators" {
  object_id           = "00000000-0000-0000-0000-000000000000"
  principal_name      = "Azure AD Admin Group"
  principal_type      = "Group"
  resource_group_name = var.rg_name
  server_name         = azurerm_postgresql_flexible_server.pgsql.name
  tenant_id           = data.azurerm_client_config.current.tenant_id
}

provider "postgresql" {
  host                = azurerm_postgresql_flexible_server.pgsql.fqdn
  port                = 5432
  database            = "postgres"
  username            = azurerm_postgresql_flexible_server_active_directory_administrator.administrators.principal_name
  sslmode             = "require"
  azure_identity_auth = true
  azure_tenant_id     = data.azurerm_client_config.current.tenant_id
}
```

### SOCKS5 Proxy Support

The provider supports connecting via a SOCKS5 proxy, but when the `postgres` scheme is used. It can be configured by setting the `ALL_PROXY` or `all_proxy` environment variable to a value like `socks5://127.0.0.1:1080`.

The `NO_PROXY` or `no_proxy` environment can also be set to opt out of proxying for specific hostnames or ports.

[libpq]: https://pkg.go.dev/github.com/lib/pq
