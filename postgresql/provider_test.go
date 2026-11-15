package postgresql

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	testAccCheckNoDefaultACLs(t)
}

func TestProviderConfigureBinaryParameters(t *testing.T) {
	tests := []struct {
		name string
		env  string
		raw  map[string]any
		want bool
	}{
		{name: "default", want: true},
		{name: "explicit true", raw: map[string]any{"binary_parameters": true}, want: true},
		{name: "explicit false", raw: map[string]any{"binary_parameters": false}, want: false},
		{name: "env false", env: "false", want: false},
		{name: "env true", env: "true", want: true},
		{name: "explicit false overrides env", env: "true", raw: map[string]any{"binary_parameters": false}, want: false},
		{name: "explicit true overrides env", env: "false", raw: map[string]any{"binary_parameters": true}, want: true},
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

func TestProviderConfigureMaxIdleConnections(t *testing.T) {
	tests := []struct {
		name string
		env  string
		raw  map[string]any
		want int
	}{
		{name: "default", want: 5},
		{name: "explicit zero", raw: map[string]any{"max_idle_connections": 0}, want: 0},
		{name: "explicit value", raw: map[string]any{"max_idle_connections": 10}, want: 10},
		{name: "env zero", env: "0", want: 0},
		{name: "env value", env: "4", want: 4},
		{name: "explicit zero overrides env", env: "4", raw: map[string]any{"max_idle_connections": 0}, want: 0},
		{name: "explicit value overrides env", env: "0", raw: map[string]any{"max_idle_connections": 3}, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PGMAXIDLECONNECTIONS", tt.env)
			d := schema.TestResourceDataRaw(t, Provider().Schema, tt.raw)

			client, err := providerConfigure(d)
			if err != nil {
				t.Fatalf("providerConfigure: %v", err)
			}
			if got := client.(*Client).config.MaxIdleConns; got != tt.want {
				t.Errorf("MaxIdleConns = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderMaxIdleConnectionsEnvDefault(t *testing.T) {
	s := Provider().Schema["max_idle_connections"]

	t.Setenv("PGMAXIDLECONNECTIONS", "")
	if v, err := s.DefaultValue(); err != nil || v != defaultProviderMaxIdleConnections {
		t.Errorf("without PGMAXIDLECONNECTIONS: DefaultValue() = %v, %v; want %d", v, err, defaultProviderMaxIdleConnections)
	}

	t.Setenv("PGMAXIDLECONNECTIONS", "4")
	if v, err := s.DefaultValue(); err != nil || v != "4" {
		t.Errorf("with PGMAXIDLECONNECTIONS=4: DefaultValue() = %#v, %v; want \"4\"", v, err)
	}
}

// TestGetRDSAuthTokenUsesRegionWithProfile checks that aws_rds_iam_region is
// used to sign the token whether or not aws_rds_iam_profile is set. The token
// is presigned locally from static credentials, so nothing goes on the wire.
func TestGetRDSAuthTokenUsesRegionWithProfile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile p]\n\n[profile q]\nregion = eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsFile, []byte(
		"[p]\naws_access_key_id = AKIDPROFILEP\naws_secret_access_key = secret\n\n"+
			"[q]\naws_access_key_id = AKIDPROFILEQ\naws_secret_access_key = secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		profile string
		region  string
		envKey  string
		wantKey string
	}{
		{name: "profile without region", profile: "p", region: "ap-southeast-1", wantKey: "AKIDPROFILEP"},
		{name: "region overrides profile region", profile: "q", region: "ap-southeast-1", wantKey: "AKIDPROFILEQ"},
		{name: "region without profile", region: "ap-southeast-1", envKey: "AKIDENV", wantKey: "AKIDENV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_CONFIG_FILE", configFile)
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsFile)
			t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
			for _, k := range []string{
				"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
				"AWS_SESSION_TOKEN", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
				"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
			} {
				t.Setenv(k, "")
			}
			t.Setenv("AWS_ACCESS_KEY_ID", tc.envKey)
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			if tc.envKey != "" {
				t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
			}

			token, _, err := getRDSAuthToken(context.Background(), tc.region, tc.profile, "", "iam_user", "db.example.com", 5432)
			if err != nil {
				t.Fatalf("getRDSAuthToken: %v", err)
			}
			_, rawQuery, ok := strings.Cut(token, "?")
			if !ok {
				t.Fatalf("token has no query: %q", token)
			}
			query, err := url.ParseQuery(rawQuery)
			if err != nil {
				t.Fatalf("parse token query: %v", err)
			}
			// AKID/date/region/service/aws4_request
			scope := strings.Split(query.Get("X-Amz-Credential"), "/")
			if len(scope) != 5 {
				t.Fatalf("unexpected credential scope %q", query.Get("X-Amz-Credential"))
			}
			if scope[0] != tc.wantKey {
				t.Errorf("access key = %q, want %q", scope[0], tc.wantKey)
			}
			if scope[2] != tc.region {
				t.Errorf("signing region = %q, want %q", scope[2], tc.region)
			}
		})
	}
}
