package postgresql

import (
	"context"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

var testAccProviders map[string]*schema.Provider
var testAccProvider *schema.Provider

func init() {
	testAccProvider = Provider()
	testAccProviders = map[string]*schema.Provider{
		"postgresql": testAccProvider,
	}
}

func TestProvider(t *testing.T) {
	if err := Provider().InternalValidate(); err != nil {
		t.Fatalf("err: %s", err)
	}
}

func TestProvider_impl(t *testing.T) {
	var _ = Provider()
}

func testAccPreCheck(t *testing.T) {
	var host string
	if host = os.Getenv("PGHOST"); host == "" {
		t.Fatal("PGHOST must be set for acceptance tests")
	}
	if v := os.Getenv("PGUSER"); v == "" {
		t.Fatal("PGUSER must be set for acceptance tests")
	}

	err := testAccProvider.Configure(context.Background(), terraform.NewResourceConfigRaw(nil))
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderMaxIdleConnectionsEnvDefault(t *testing.T) {
	s := Provider().Schema["max_idle_connections"]

	t.Setenv("PGMAXIDLECONNECTIONS", "")
	if v, err := s.DefaultValue(); err != nil || v != 0 {
		t.Errorf("without PGMAXIDLECONNECTIONS: DefaultValue() = %v, %v; want 0", v, err)
	}

	t.Setenv("PGMAXIDLECONNECTIONS", "4")
	if v, err := s.DefaultValue(); err != nil || v != "4" {
		t.Errorf("with PGMAXIDLECONNECTIONS=4: DefaultValue() = %#v, %v; want \"4\"", v, err)
	}
}

func TestProviderMaxTotalConnectionsEnvDefault(t *testing.T) {
	s := Provider().Schema["max_total_connections"]
	if s == nil {
		t.Fatal("provider has no max_total_connections argument")
	}

	t.Setenv("PGMAXTOTALCONNECTIONS", "")
	if v, err := s.DefaultValue(); err != nil || v != 0 {
		t.Errorf("without PGMAXTOTALCONNECTIONS: DefaultValue() = %v, %v; want 0", v, err)
	}

	t.Setenv("PGMAXTOTALCONNECTIONS", "10")
	if v, err := s.DefaultValue(); err != nil || v != "10" {
		t.Errorf("with PGMAXTOTALCONNECTIONS=10: DefaultValue() = %#v, %v; want \"10\"", v, err)
	}
}

func TestProviderConfigureBinaryParameters(t *testing.T) {
	tests := []struct {
		name string
		env  string
		raw  map[string]any
		want bool
	}{
		{name: "default", want: false},
		{name: "enabled", raw: map[string]any{"binary_parameters": true}, want: true},
		{name: "env", env: "true", want: true},
		{name: "explicit false overrides env", env: "true", raw: map[string]any{"binary_parameters": false}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PGBINARYPARAMETERS", tt.env)
			d := schema.TestResourceDataRaw(t, Provider().Schema, tt.raw)

			client, err := providerConfigure(d)
			if err != nil {
				t.Fatalf("providerConfigure: %v", err)
			}
			if got := client.(*Client).config.BinaryParameters; got != tt.want {
				t.Errorf("BinaryParameters = %v, want %v", got, tt.want)
			}
		})
	}
}
