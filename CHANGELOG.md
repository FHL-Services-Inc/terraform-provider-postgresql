# Changelog

All notable changes to this fork
([FHL-Services-Inc/terraform-provider-postgresql](https://github.com/FHL-Services-Inc/terraform-provider-postgresql))
are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Numbers refer to pull
requests and issues in this repository.

Releases up to v1.27.0 were made upstream; see the
[upstream releases](https://github.com/cyrilgdn/terraform-provider-postgresql/releases/).

## [Unreleased]

### Changed

- **Docs**: the README links to this fork's page on the Terraform Registry
  instead of the upstream provider's, and its build instructions clone this
  repository. The examples and the `tests/bench` scripts use the
  `FHL-Services-Inc/postgresql` source address (#215).

## [1.29.1] - 2026-11-24

### Fixed

- **Connection**: an attempt to connect is no longer abandoned 30 s after
  `connection_retry_timeout_seconds` (5 s by default). Since v1.28.0 a server
  that took longer than about 35 s to accept a connection failed with
  "timeout while waiting for state to become 'success'", even without retries
  and with the default `connect_timeout` of 180 s. `connect_timeout` limits
  each attempt again, and the retry timeout only decides whether a new attempt
  may start; it plays no part with `max_conn_retries = 0`, the default (#216).
- **Extension members in grants on all objects of a schema**: with
  `objects` empty, the tables, sequences and routines that belong to an
  extension installed in the schema are left out of `postgresql_grant`
  (#217). With a non-superuser provider such a grant failed since v1.29.0
  when the members belong to a superuser, as `pg_stat_statements` does on
  Amazon RDS (`rdsadmin`): "is a superuser and cannot be granted". Their
  privileges are no longer read, so they do not show as drift, and when the
  schema holds members the `GRANT` and `REVOKE` list the other objects
  instead of using `ON ALL ... IN SCHEMA`. List an extension member in
  `objects` to manage its privileges.

## [1.29.0] - 2026-11-15

### Upgrade notes

These changes can show up as a diff or an error on the first plan or apply after
upgrading. Each one fixes behavior that was silently wrong before.

- **New connection defaults: `max_idle_connections = 5` and
  `binary_parameters = true`** (#205, #202). Each database pool now keeps up
  to 5 idle connections for reuse (closed 30 s after their last use or when
  Terraform exits), and statements with bind parameters take one round-trip
  instead of two. What you may notice:
  - more sessions on the server while Terraform runs: about
    `(databases + 1) × 5` per provider block (measured peaks at
    `-parallelism=10`: 15 with 1 database, 26–29 with 4, against 10 before).
    Check the server's and the role's connection limits, or cap the total with
    `max_total_connections`;
  - a `DROP DATABASE`, `ALTER DATABASE ... RENAME` or
    `CREATE DATABASE ... TEMPLATE` run outside of Terraform, on a database the
    provider holds idle connections to, can fail with "database is being
    accessed by other users" while Terraform runs. `postgresql_database`
    closes the provider's own pools before such operations.

  To keep the previous behavior, set `max_idle_connections = 0` and
  `binary_parameters = false` in the provider block, or
  `PGMAXIDLECONNECTIONS=0` and `PGBINARYPARAMETERS=false` in the environment
  (a value in the provider block takes precedence).
- **`postgresql_grant` on superuser-owned objects fails without `WITH GRANT OPTION`**
  (#201, #114). With a non-superuser provider, a grant on objects owned by a
  superuser (a schema created `AUTHORIZATION postgres`, `public` before
  PostgreSQL 15, a database owned by `postgres`) now fails unless the provider
  holds the managed privileges `WITH GRANT OPTION`. Before, PostgreSQL ignored
  the statement with a warning and the apply succeeded without granting
  anything.
- **Removing a schema `policy` revokes the privilege itself** (#200, #184). A
  policy held `WITH GRANT OPTION` used to lose only the grant option; the role
  now loses `USAGE`/`CREATE`. Removing every `policy` block still revokes
  nothing (`policy` is optional and computed).
- **`postgresql_grant` with `object_type = "table"` and empty `objects`** also
  covers views, materialized views, partitioned and foreign tables (#141, #116),
  which is what `GRANT ... ON ALL TABLES IN SCHEMA` grants. The first plan can
  show changes for such relations created after the grant.
- **Default ACLs are read for objects that were never granted on** (#95, #77):
  a NULL ACL now reads as the built-in default instead of "no privileges". For
  example, a `role = "public"`, `privileges = []` grant on functions shows drift
  when a new function (which gets `EXECUTE` for `PUBLIC` by default) appears.
- **Function grants check signatures and routine kinds on refresh** (#73, #63,
  #78): an out-of-band `REVOKE EXECUTE` on a listed signature now shows as
  drift.
- **Grant drift stays visible across refreshes** (#101, #76). When the covered
  objects have different privileges, the plan marks the entries
  (`"X (on some objects only)"`) instead of showing one object's set.
- **Destroying a global `postgresql_default_privileges` restores the built-in
  defaults** for the owner itself, and for `PUBLIC` on functions, routines and
  types (#191, #182), instead of leaving them without privileges on new
  objects. Keep a resource with `privileges = []` to keep them revoked.
- **`postgresql_role.roles` applies only the added and removed roles** (#104,
  #93). A membership granted outside Terraform after the last refresh survives
  an update.
- **Deleting a role reassigns and drops its owned objects in every database**
  of the cluster, not only in the provider's database (#175, #137).
- **Role passwords, database options and publication names containing `\` are
  stored exactly as configured** (#72, #65). A password with a backslash takes
  effect the next time it is written; anyone who logged in with the doubled
  backslash form needs to switch to the configured value.
- **An explicit `aws_rds_iam_region` takes precedence** over the region of
  `aws_rds_iam_profile` and the environment (#195, #143). Before, it was ignored
  whenever a profile was set.
- **`database = ""` on `postgresql_grant` is rejected at plan time** (#174,
  #136) instead of failing at apply.
- **A failed `postgresql_role` update keeps the previous state** (#176, #119).
- **`postgresql_security_label` has a `database` attribute and a new ID**,
  `<database>.<provider>.<object type>.<object name>` (#206, #199). IDs in
  the state are rewritten on the next refresh without a diff, and import uses
  the new form. `object_type` is validated and must be lower case; an upper
  case type was never read back before.
- **`statement_timeout = 0` on `postgresql_role` sets the timeout to 0** (#207,
  #134). Before, 0 reset the setting, so the role inherited a database or
  server value instead of disabling it, and adding or removing `= 0` on an
  existing role showed no change. Roles with an explicit `0` (also for
  `idle_in_transaction_session_timeout`) show an in-place update on the first
  plan, and the role then overrides any database or server value with 0;
  remove the attribute to keep inheriting it. Both attributes are strings now
  and accept units (`"30s"`); numbers in configurations keep working, and
  existing states are upgraded without a diff.

### Added

- Performance settings, documented in the new
  [performance tuning guide](website/docs/guides/performance-tuning.html.markdown)
  (#153, #162):
  - `max_idle_connections` / `PGMAXIDLECONNECTIONS`: reuse connections instead
    of reconnecting for every statement (#17, #7, #23). Idle pools are evicted
    before a database rename or template copy, and idle connections close after
    30 s (#55, #24). Defaults to `5` (#205, #202); `0` turns it off.
  - `binary_parameters` / `PGBINARYPARAMETERS`: one round-trip per
    parameterized statement (#45, #36). Enabled by default (#205, #202);
    `false` turns it off.
  - `catalog_snapshot` / `PGCATALOGSNAPSHOT` (and `catalog_snapshot_ttl_seconds`), opt-in: read
    `postgresql_grant` and `postgresql_default_privileges` from one catalog
    snapshot per database (#98, #67).
  - `max_total_connections` / `PGMAXTOTALCONNECTIONS`, opt-in: one limit for the
    connections across all database pools (#185, #57).
- Import for `postgresql_grant`, `postgresql_default_privileges` and
  `postgresql_grant_role` (#83, #69), `postgresql_user_mapping` (#168, #166),
  `postgresql_server` by name (#188, #124) and `postgresql_security_label`
  (#197, #189).
- `postgresql_security_label`: `database` attribute (the provider database by
  default), and `label_provider` can be omitted when one provider is loaded
  (#206, #199).
- `postgresql_user_mapping` supports `CURRENT_USER`, `CURRENT_ROLE` and `USER`
  (#170, #167) and reads back mappings for `PUBLIC` (#165, #121, #150).
- `statement_timeout` and `idle_in_transaction_session_timeout` on
  `postgresql_role` accept time units (#172, #120).
- RDS IAM and Azure AD tokens are refreshed for every new connection instead
  of once per provider run (#81, #13), also with `scheme = "awspostgres"`,
  which used the first token for the whole run and failed after 15 minutes
  (#214, #89). The AWS config, and the assumed role credentials with
  `aws_rds_iam_provider_role_arn`, are loaded once and reused.

### Performance

With connection reuse and single round-trip statements turned off (the
defaults before #205), a plan of the benchmark configuration (1,143
resources, 10 ms round-trip time) went from 39.9 s to 14.6 s; with
`max_idle_connections = 10` and `binary_parameters` (both now on by default,
`max_idle_connections` at `5`) it takes 3.7 s, and 2.4 s with
`catalog_snapshot` too (#33).

- With `scheme = "awspostgres"` the RDS CA bundle is downloaded once per
  provider run instead of once per connection, with IAM and with password
  authentication (#214, #97, #102).
- `postgresql_grant` and `postgresql_default_privileges` are read with one
  statement, without a transaction or read lock (#51, #38, #52); the `objects`
  filter of table, sequence and function grants runs in SQL (#61, #59).
- Single-statement Reads run without an explicit transaction (#39, #37, #43),
  as do publication and subscription Reads (#50, #44).
- Fewer catalog round-trips for role checks: role existence and OID in one
  query, batched superuser checks (#15, #9), session user and `rolsuper`
  memoized per pool (#54, #47).
- Fixed a regression from v1.28.0 where reading a table grant with `objects`
  expanded the ACLs of every relation in the database (#40, #35).

### Fixed

- **Concurrency and locking**
  - `postgresql_database` owner handling runs on a single connection: no more
    deadlock with a small `max_connections` (#41, #31).
  - Role advisory locks are taken in one statement ordered by OID, so roles
    that share members no longer deadlock (#112, #111).
  - Grant writes lock the granted objects: parallel grants on the same objects
    wait instead of failing with `tuple concurrently updated`; grants no longer
    lock members of the grantee (#99, #26, #53).
  - Cross-database membership changes (owner changes, temporary memberships)
    are serialized (#144, #25), also for `postgresql_database` (#178, #152).
  - `REASSIGN OWNED` (a database owner change with `alter_object_ownership`,
    a role delete) and grant or default privileges writes in the same
    database are serialized: a grant no longer takes the membership of an
    owner the objects are being reassigned away from and fails with
    `permission denied` (#213, #181).
- **Grants**
  - Owners of the granted objects (sequences, functions, views, foreign tables)
    are looked up instead of only table owners; `rows.Err()` is checked
    (#48, #32).
  - Owners the provider cannot become a member of (PostgreSQL 16+, indirect
    reverse membership) are handled (#179, #132, #133).
  - Database grants take the database owner and run on the maintenance
    database (#147, #113, #169, #148).
  - Removing a table, sequence or column grant no longer fails when some of the
    listed relations were dropped outside Terraform (#198, #115).
- **`postgresql_grant_role`**: the read picks the membership row granted by
  the current user, or by the bootstrap superuser for superuser providers, on
  PostgreSQL 16+ (#146, #138, #163, #151).
- **`postgresql_default_privileges`**: a global entry equal to the built-in
  defaults no longer fails with "inconsistent result after apply" (#177, #117).
- **`postgresql_database`**
  - Changing the owner of a database owned by another role works for a
    non-superuser provider (#49, #42); the new owner is granted before
    `REASSIGN OWNED` (#142, #131).
  - `encoding`, `lc_collate`, `lc_ctype` and `tablespace_name` set to
    `DEFAULT` no longer replace or update the database on every plan (#173,
    #128).
  - Deleting the provider's own database no longer closes the pool the other
    resources run on (#164, #149).
- **`postgresql_schema`**: owner change and rename take the needed memberships
  for a non-superuser provider; `pg_database_owner` is resolved to the
  database owner (#194, #118, #183).
- **`postgresql_publication`**: name and owner are quoted; the imported ID
  keeps the name (#186, #122, #123).
- **`postgresql_server`**: option values containing `=` are read back intact
  (#196, #190).
- **`postgresql_security_label`**: schema-qualified names, objects outside the
  `search_path`, columns (`table.column`), functions with or without their
  argument types and provider names that need quoting are labelled and read
  back; the label is read by the OID of the object. Ported from the
  [cheeeee fork](https://github.com/cheeeee/terraform-provider-postgresql)
  (#206, #199).
- **Failed renames**: when a rename of a `postgresql_schema`,
  `postgresql_publication`, `postgresql_server` or `postgresql_role` fails (or
  the update fails after the rename), the state keeps the previous name and ID.
  Before, the next refresh lost the object or took over another object that
  already had the new name, which destroy then dropped (#203, #193).
- **`postgresql_function`**: argument defaults and types containing commas are
  read from the catalog; IDs are parsed up to the argument list, so argument
  types can be schema-qualified (#192, #126, #127).
- **`postgresql_function`** in a schema owned by another role works for a
  non-superuser provider: create, replace and drop take the owner of the
  schema and of the function when the provider lacks the privilege, and the
  function is read from the catalog without `USAGE` on its schema (#212,
  #209).
- **Data sources**: `postgresql_tables` and `postgresql_sequences` read from
  `pg_class` (#187, #125, #139); pattern filters are bound instead of
  interpolated (#75, #74).
- **Connection**: user names with `:`, IPv6 hosts and password redaction in
  the DSN (#171, #129, #130, #135); the server version is read from
  `server_version_num`, so versions such as `13.23-rds` parse (#180, #140).

### Documentation

- Performance tuning guide: connection reuse, connection limits, binary
  parameters, catalog snapshot, configuration layout and benchmark numbers
  (#153, #162).
- Amazon RDS and Aurora: owner roles with `rds_iam` break the password logins
  of the provider while it is a temporary member of them; use a separate login
  role with `rds_iam` (#208, #145).

### Tests and CI

- Acceptance tests are sharded; pull requests run PostgreSQL 12, 16 and 18,
  `main`, nightly runs and the `full-matrix` label run 12–18 (#157, #156).
- Reproducible large-plan benchmark in `tests/bench` (#34, #18, #58).
- Local acceptance runs with Apple `container` and stock PostgreSQL images
  (#80, #28); `make lint` pinned to the CI linter version (#30, #29).
- Test isolation: unique names for cluster-wide objects, no default privileges
  left in `postgres`, shared database teardown (#106, #86, #110, #109, #155,
  #154).
- The remove-waiting-response workflow no longer fails when the label is
  absent (#161, #159).
- The test teardown no longer fails in the non-superuser profile when an
  autovacuum worker is connected to the database it drops (#211).

## [1.28.0] - 2026-11-13

First release from this fork.

### Changed

- Redundant `Exists` callbacks removed; resources detect objects that are gone
  in `Read` (#2).
- Fewer round-trips in the `postgresql_database` and `postgresql_grant_role`
  Reads (#3).
- The `objects` filter of table and sequence grants runs in SQL (#4).

### Fixed

- `postgresql_physical_replication_slot`: slots dropped outside Terraform are
  detected (#2).

### Added

- Terraform Registry manifest (#1), release signing key (#5) and release
  workflow permissions (#6).

### Upstream changes since v1.27.0

- `postgresql_replication_slot` no longer uses a transaction, so it works on a
  replica (cyrilgdn/terraform-provider-postgresql#648).
- Connection retry logic (cyrilgdn/terraform-provider-postgresql#576).

[Unreleased]: https://github.com/FHL-Services-Inc/terraform-provider-postgresql/compare/v1.29.1...HEAD
[1.29.1]: https://github.com/FHL-Services-Inc/terraform-provider-postgresql/compare/v1.29.0...v1.29.1
[1.29.0]: https://github.com/FHL-Services-Inc/terraform-provider-postgresql/compare/v1.28.0...v1.29.0
[1.28.0]: https://github.com/FHL-Services-Inc/terraform-provider-postgresql/compare/v1.27.0...v1.28.0
