/*
Copyright The Zabbix Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package zabbix

import (
	"strings"
	"testing"
)

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBuiltin(t *testing.T) {
	c := Builtin()
	for _, s := range []string{"7.0.1", "7.2.15", "7.4.7", "8.0.0rc1"} {
		if l, ok := c.Lookup(mustVersion(t, s)); !ok || !l.Verified {
			t.Errorf("%s must be a verified built-in line", s)
		}
	}
	for _, s := range []string{"6.0.40", "7.1.0", "7.3.0", "9.0.0"} {
		if _, ok := c.Lookup(mustVersion(t, s)); ok {
			t.Errorf("%s must not be supported", s)
		}
	}
	want := "Zabbix 9.0 is not supported by this operator version (supported lines: 7.0, 7.2, 7.4, 8.0)"
	if got := c.UnsupportedMessage(mustVersion(t, "9.0.0")); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	l80, _ := c.Lookup(mustVersion(t, "8.0.0"))
	l74, _ := c.Lookup(mustVersion(t, "7.4.7"))
	for _, tc := range []struct {
		line Line
		pg   int
		want string
	}{
		{l80, 14, "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)"},
		{l80, 15, ""},
		{l80, 18, ""},
		{l80, 19, "PostgreSQL 19 is too new for Zabbix 8.0 (supports up to 18)"},
		{l74, 12, "PostgreSQL 12 is too old for Zabbix 7.4 (needs 13 or newer)"},
		{l74, 18, ""},
	} {
		if got := tc.line.PostgresProblem(tc.pg); got != tc.want {
			t.Errorf("%s on PostgreSQL %d: %q, want %q", tc.line.Line, tc.pg, got, tc.want)
		}
	}
}

func TestParseCompatibility(t *testing.T) {
	c, err := ParseCompatibility([]byte(`
lines:
  - line: "8.2"
    minPostgres: 15
    maxPostgres: 18
  - line: "8.0"          # same limits as built in: stays verified
    minPostgres: 15
    maxPostgres: 18
  - line: "7.0"          # changed limits: no longer verified
    minPostgres: 14
`))
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := c.Lookup(mustVersion(t, "8.2.1")); !ok || l.Verified || l.MinPostgres != 15 || l.MaxPostgres != 18 {
		t.Errorf("8.2 = %+v, %v", l, ok)
	}
	if l, _ := c.Lookup(mustVersion(t, "8.0.0")); !l.Verified {
		t.Error("8.0 with unchanged limits must stay verified")
	}
	if l, _ := c.Lookup(mustVersion(t, "7.0.25")); l.Verified || l.MinPostgres != 14 {
		t.Errorf("7.0 = %+v", l)
	}
	if l, _ := c.Lookup(mustVersion(t, "7.4.7")); !l.Verified {
		t.Error("lines not configured stay built in")
	}
	if got := strings.Join(c.Lines(), ","); got != "7.0,7.2,7.4,8.0,8.2" {
		t.Errorf("lines %s", got)
	}
	if c, err := ParseCompatibility(nil); err != nil || len(c.Lines()) != 4 {
		t.Errorf("empty configuration: %v, %v", c, err)
	}

	for in, msg := range map[string]string{
		"lines:\n  - line: \"8\"\n    minPostgres: 15":                                           "must look like",
		"lines:\n  - line: \"8.2\"\n    minPostgres: 9":                                          "minPostgres",
		"lines:\n  - line: \"8.2\"\n    minPostgres: 15\n    maxPostgres: 14":                    "maxPostgres",
		"lines:\n  - line: \"8.2\"\n    minPostgres: 15\n  - line: \"8.2\"\n    minPostgres: 15": "more than once",
		"lines:\n  - line: \"8.2\"\n    minPostgress: 15":                                        "unknown field",
		"line: 8.2": "unknown field",
	} {
		if _, err := ParseCompatibility([]byte(in)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("ParseCompatibility(%q) error = %v, want %q", in, err, msg)
		}
	}
}
