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

package jobs

import (
	"testing"

	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

func TestDecide(t *testing.T) {
	pg := func(major int) int { return major * 10000 }
	cases := []struct {
		name   string
		f      facts
		target string
		ok     bool
		reason string
		change zabbix.Change
		msg    string
	}{
		{"fresh install on 14", facts{versionNum: pg(14)}, "7.0.1", true, "", zabbix.FreshInstall,
			"PostgreSQL 14, empty database: the schema will be created for Zabbix 7.0.1"},
		{"patch upgrade", facts{versionNum: pg(15), mandatory: 7000000, activeNodes: 1}, "7.0.25", true, "", zabbix.SameSchema,
			"PostgreSQL 15, schema 7000000 already matches Zabbix 7.0.25"},
		{"major upgrade on 15", facts{versionNum: pg(15), mandatory: 7000000}, "8.0.0rc1", true, "", zabbix.SchemaUpgrade,
			"PostgreSQL 15, schema 7000000 will be upgraded for Zabbix 8.0.0rc1"},
		{"8.0 on 14 is blocked", facts{versionNum: pg(14), mandatory: 7000000}, "8.0.0rc1", false, "PostgreSQLTooOld", "",
			"PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)"},
		{"7.0 on 12 is blocked", facts{versionNum: pg(12)}, "7.0.1", false, "PostgreSQLTooOld", "",
			"PostgreSQL 12 is too old for Zabbix 7.0 (needs 13 or newer)"},
		{"replica", facts{versionNum: pg(17), inRecovery: true, mandatory: 7000000}, "7.0.25", false, "NotPrimary", "",
			"the database is in recovery; connect to the primary"},
		{"downgrade", facts{versionNum: pg(17), mandatory: 7050195}, "7.0.25", false, "Downgrade", zabbix.Downgrade,
			"the database schema (dbversion 7050195) is newer than Zabbix 7.0.25; Zabbix cannot downgrade"},
		{"8.0 on 19 is blocked", facts{versionNum: pg(19), mandatory: 7000000}, "8.0.0", false, "PostgreSQLTooNew", "",
			"PostgreSQL 19 is too new for Zabbix 8.0 (supports up to 18)"},
		{"7.4 on 17", facts{versionNum: pg(17), mandatory: 7040000}, "7.4.7", true, "", zabbix.SameSchema,
			"PostgreSQL 17, schema 7040000 already matches Zabbix 7.4.7"},
		{"replica before old postgres", facts{versionNum: pg(14), inRecovery: true}, "8.0.0rc1", false, "NotPrimary", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := zabbix.ParseVersion(c.target)
			if err != nil {
				t.Fatal(err)
			}
			line, ok := zabbix.Builtin().Lookup(v)
			if !ok {
				t.Fatalf("line of %s not built in", c.target)
			}
			r := decide(c.f, v, line)
			if r.OK != c.ok || r.Reason != c.reason || r.Change != string(c.change) {
				t.Errorf("got ok=%v reason=%q change=%q (%s)", r.OK, r.Reason, r.Change, r.Message)
			}
			if c.msg != "" && r.Message != c.msg {
				t.Errorf("message %q, want %q", r.Message, c.msg)
			}
			if r.PostgresMajor != c.f.versionNum/10000 {
				t.Errorf("PostgresMajor %d", r.PostgresMajor)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	for reason, want := range map[string]bool{"DatabaseUnreachable": true, "DatabaseError": true, "Configuration": true,
		"PostgreSQLTooOld": false, "LiveNodes": false, "Downgrade": false, "": false} {
		if got := (Result{Reason: reason}).Retryable(); got != want {
			t.Errorf("%q: Retryable = %v, want %v", reason, got, want)
		}
	}
}
