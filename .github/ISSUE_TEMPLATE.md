### Versions

- Terraform (`terraform -v`):
- Provider version and source address (e.g. `1.29.1`, `FHL-Services-Inc/postgresql`):
- PostgreSQL version (`SELECT version();`):
- Server: self-hosted / Amazon RDS / Aurora / Azure / Google Cloud SQL / other (which?):
- Does the provider connect as a superuser (`superuser` in the provider block, or the role's `rolsuper`)?

### Affected resource(s) or data source(s)

- postgresql_...

### Configuration

A minimal configuration that reproduces the problem. Leave out passwords and other secrets.

```hcl
```

### Expected behavior

### Actual behavior

Include the error message, if any.

### Steps to reproduce

1. `terraform apply`

### Debug output

Run with `TF_LOG=DEBUG` and paste the relevant part here or link a Gist.
Check the output for passwords and connection strings and remove them first.
