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
* `connect_timeout` - (Optional) Maximum wait for connection, in seconds. The
  default is `180s`.  Zero or not specified means wait indefinitely.
* `max_conn_retries` - (Optional) Maximum number of connection retries. Zero
  means no retries. The default is `0`.
* `connection_retry_timeout_seconds` - (Optional) Maximum total wait, in
  seconds, across all connection retries. The default is `5`.
* `max_connections` - (Optional) Set the maximum number of open connections to
  the database. The default is `20`.  Zero means unlimited open connections.
  The provider keeps a separate pool for each database it connects to, so this
  limit applies per database; use `max_total_connections` for a limit across
  all databases.
* `max_idle_connections` - (Optional) Maximum number of idle connections kept
  open for reuse, per database the provider connects to. The default is `0`:
  every connection is closed once it is returned, so each query opens a new
  connection. Setting it above zero avoids a new connection (TCP, TLS and
  authentication handshake) for most queries, which noticeably speeds up plans
  and applies with many resources. Idle connections are closed after 30
  seconds. Each database the provider connects to gets its own pool, so the
  limit applies per database, not in total. `postgresql_database` closes the provider's own connections to a
  database before dropping or renaming it, or before using it as the
  `template` of another database. While Terraform runs, a plain
  `DROP DATABASE`, `ALTER DATABASE ... RENAME` or `CREATE DATABASE ... TEMPLATE`
  issued outside of the provider can fail with "database is being accessed by
  other users" because of these idle connections; terminate them first
  (`pg_terminate_backend`) or use `DROP DATABASE ... WITH (FORCE)` on
  PostgreSQL 13+. The value is capped to `max_connections` when the latter is
  non-zero. It can also be set with the `PGMAXIDLECONNECTIONS` environment
  variable.
* `max_total_connections` - (Optional) Maximum number of connections open at
  once to the server, across all the databases the provider connects to. The
  limit is shared by every provider configuration (alias) that connects to the
  same host and port as the same user. The default is `0`: no limit besides
  `max_connections` per database. Without it, the connections the provider
  holds add up across databases: with `max_idle_connections` set, each
  database's pool can keep up to `max_connections` connections, so peak usage
  grows with the number of databases. When the limit is reached, the provider
  closes an idle connection of another database to open the one it needs, and
  otherwise waits for one to be returned. If none becomes available within
  `connect_timeout` (180 seconds when `connect_timeout` is zero), the operation
  fails with an error instead of waiting forever; raise the limit or lower
  `terraform -parallelism`. Setting it also caps `max_connections` to the same
  value. With pooling, set `max_idle_connections = max_total_connections`.
  Each connection belongs to one database, so a limit close to or below
  `-parallelism` makes the provider close and reopen connections to move them
  between databases; about twice `-parallelism` keeps almost all of the reuse.
  Only supported with the `postgres`
  scheme; ignored for `awspostgres` and `gcppostgres`. It can also be set with
  the `PGMAXTOTALCONNECTIONS` environment variable.
* `conn_max_lifetime_seconds` - (Optional) Maximum lifetime of a connection,
  in seconds. The default is `0`.  Zero means unlimited.
* `advisory_lock_timeout` - (Optional) Maximum wait, in milliseconds, for the
  advisory locks the provider takes on a role (and its members) or a database
  before changing grants, default privileges, roles and database ownership.
  The default is `0`, which waits indefinitely (previous behavior). When set,
  an apply that cannot get the lock in time fails with an error naming the
  locked role or database instead of hanging, e.g. when another Terraform run
  or session holds it. Only the lock acquisition is bounded; the statements
  that follow keep their usual lock waits. Resources in the same apply also
  queue on these locks (e.g. every `postgresql_database` with an `owner` locks
  the provider's own role while it is created), so pick a value well above how
  long such operations take rather than a few hundred milliseconds.
* `binary_parameters` - (Optional) If set to `true`, adds `binary_parameters=yes`
  to the connection string. The driver then sends each statement that has bind
  parameters (almost every query the provider runs) in a single round-trip
  instead of two (a separate prepare step followed by the execution). This
  mostly helps when the database is far from where Terraform runs. Only
  `[]byte` parameters are sent in binary format, which the provider does not
  use, and results are still read as text. Can also be set with the
  `PGBINARYPARAMETERS` environment variable. The default is `false`.
* `expected_version` - (Optional) Specify a hint to Terraform regarding the
  expected version that the provider will be talking with.  This is a required
  hint in order for Terraform to talk with an ancient version of PostgreSQL.
  This parameter is expected to be a [PostgreSQL
  Version](https://www.postgresql.org/support/versioning/) or `current`.  Once a
  connection has been established, Terraform will fingerprint the actual
  version.  Default: `9.0.0`.
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
