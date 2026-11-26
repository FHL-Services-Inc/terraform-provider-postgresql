package main

import (
	"github.com/FHL-Services-Inc/terraform-provider-postgresql/postgresql"
	"github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
)

func main() {
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: postgresql.Provider})
}
