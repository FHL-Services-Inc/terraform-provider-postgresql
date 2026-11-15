package postgresql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/lib/pq"
)

func TestParseMillisecondsSetting(t *testing.T) {
	cases := []struct {
		value    string
		expected int
	}{
		{"120000", 120000},
		{"0", 0},
		{"12s", 12000},
		{"2min", 120000},
		{"150ms", 150},
		{"1h", 3600000},
		{"1d", 86400000},
		{"1500us", 2},
		{"1.5s", 1500},
		{"2 min", 120000},
	}
	for _, c := range cases {
		res, err := parseMillisecondsSetting(c.value)
		if err != nil {
			t.Errorf("parseMillisecondsSetting(%q) returned error: %v", c.value, err)
			continue
		}
		if res != c.expected {
			t.Errorf("parseMillisecondsSetting(%q) = %d, expected %d", c.value, res, c.expected)
		}
	}

	for _, invalid := range []string{"abc", "5fortnights", "", "min", "s"} {
		if _, err := parseMillisecondsSetting(invalid); err == nil {
			t.Errorf("parseMillisecondsSetting(%q) expected error, got none", invalid)
		}
	}
}

func TestReadRoleTimeout(t *testing.T) {
	roleConfig := pq.ByteaArray{
		[]byte("statement_timeout=12s"),
		[]byte("lock_timeout=5s"),
		[]byte("idle_in_transaction_session_timeout=0"),
	}
	cases := []struct {
		attr, current, expected string
	}{
		// The catalog value when the state has none or another duration.
		{roleStatementTimeoutAttr, "", "12s"},
		{roleStatementTimeoutAttr, "5000", "12s"},
		// The state value when it is the same duration.
		{roleStatementTimeoutAttr, "12000", "12000"},
		// 0 is a value of its own, not "unset".
		{roleIdleInTransactionSessionTimeoutAttr, "", "0"},
	}
	for _, c := range cases {
		res, err := readRoleTimeout(roleConfig, c.attr, c.current)
		if err != nil {
			t.Errorf("readRoleTimeout(%s, %q) returned error: %v", c.attr, c.current, err)
			continue
		}
		if res != c.expected {
			t.Errorf("readRoleTimeout(%s, %q) = %q, expected %q", c.attr, c.current, res, c.expected)
		}
	}

	// A setting the role does not have is unset, even if the state has a value.
	res, err := readRoleTimeout(pq.ByteaArray{[]byte("lock_timeout=5s")}, roleStatementTimeoutAttr, "30000")
	if err != nil || res != "" {
		t.Errorf("readRoleTimeout without the setting = %q, %v, expected \"\", nil", res, err)
	}
}

func TestReadRoleTimeoutErrorNamesSetting(t *testing.T) {
	roleConfig := pq.ByteaArray{[]byte("idle_in_transaction_session_timeout=bogus")}
	_, err := readRoleTimeout(roleConfig, roleIdleInTransactionSessionTimeoutAttr, "")
	if err == nil {
		t.Fatal("expected error for bogus value")
	}
	if got := err.Error(); !strings.Contains(got, "idle_in_transaction_session_timeout") {
		t.Errorf("error should name the setting being read, got: %s", got)
	}
}

func TestValidateMillisecondsSetting(t *testing.T) {
	for _, valid := range []string{"", "0", "30000", "30s", "2 min", "1500us"} {
		if _, errs := validateMillisecondsSetting(valid, "statement_timeout"); len(errs) != 0 {
			t.Errorf("validateMillisecondsSetting(%q) = %v, expected no error", valid, errs)
		}
	}
	for _, invalid := range []string{"-1", "abc", "5fortnights", "s"} {
		if _, errs := validateMillisecondsSetting(invalid, "statement_timeout"); len(errs) == 0 {
			t.Errorf("validateMillisecondsSetting(%q) expected an error", invalid)
		}
	}
}

func TestSuppressEquivalentMilliseconds(t *testing.T) {
	cases := []struct {
		old, new string
		suppress bool
	}{
		{"30000", "30s", true},
		{"2min", "120000", true},
		{"30000", "31s", false},
		// unset and 0 differ: inherit vs. no timeout
		{"", "0", false},
		{"0", "", false},
		{"", "", true},
		{"bogus", "30s", false},
	}
	for _, c := range cases {
		if got := suppressEquivalentMilliseconds("statement_timeout", c.old, c.new, nil); got != c.suppress {
			t.Errorf("suppressEquivalentMilliseconds(%q, %q) = %v, expected %v", c.old, c.new, got, c.suppress)
		}
	}
}

func TestUpgradeRoleStateV0(t *testing.T) {
	rawState := map[string]any{
		"name":                                  "r",
		roleStatementTimeoutAttr:                float64(30000),
		roleIdleInTransactionSessionTimeoutAttr: float64(0),
	}
	got, err := upgradeRoleStateV0(context.Background(), rawState, nil)
	if err != nil {
		t.Fatalf("upgradeRoleStateV0 returned error: %v", err)
	}
	if v := got[roleStatementTimeoutAttr]; v != "30000" {
		t.Errorf("statement_timeout = %#v, expected \"30000\"", v)
	}
	// 0 was what Read stored for a role without the setting.
	if v, ok := got[roleIdleInTransactionSessionTimeoutAttr]; ok {
		t.Errorf("idle_in_transaction_session_timeout = %#v, expected it to be removed", v)
	}
	if got["name"] != "r" {
		t.Errorf("name = %#v, expected it unchanged", got["name"])
	}

	// json.Number and a state that already has strings.
	got, err = upgradeRoleStateV0(context.Background(), map[string]any{
		roleStatementTimeoutAttr:                json.Number("12000"),
		roleIdleInTransactionSessionTimeoutAttr: "5s",
	}, nil)
	if err != nil {
		t.Fatalf("upgradeRoleStateV0 returned error: %v", err)
	}
	if got[roleStatementTimeoutAttr] != "12000" || got[roleIdleInTransactionSessionTimeoutAttr] != "5s" {
		t.Errorf("upgradeRoleStateV0 = %#v", got)
	}

	// The type the SDK converts version 0 states with has the timeouts as numbers.
	ty := resourcePostgreSQLRoleV0().CoreConfigSchema().ImpliedType()
	if at := ty.AttributeType(roleStatementTimeoutAttr); !at.Equals(cty.Number) {
		t.Errorf("version 0 statement_timeout type = %s, expected number", at.FriendlyName())
	}
}
