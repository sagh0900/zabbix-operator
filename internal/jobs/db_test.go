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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Values with quotes, backslashes and spaces survive the connection string.
func TestConnStringQuotesValues(t *testing.T) {
	c := DBConfig{Host: "pg-rw", Port: 5432, Name: "zab'bix", User: `us\er`, SSLMode: "disable"}
	cfg, err := pgx.ParseConfig(c.connString())
	if err != nil {
		t.Fatalf("connection string does not parse: %v", err)
	}
	if cfg.Database != "zab'bix" || cfg.User != `us\er` || cfg.Host != "pg-rw" || cfg.Port != 5432 {
		t.Errorf("parsed %s/%s@%s:%d", cfg.Database, cfg.User, cfg.Host, cfg.Port)
	}
	if strings.Contains(c.connString(), "password") {
		t.Error("the password must never be in the connection string")
	}
	// pgx opens certificate files while parsing, so paths are checked on the string.
	c.SSLRootCert = "/etc/certs dir/ca.crt"
	if !strings.Contains(c.connString(), "sslrootcert='/etc/certs dir/ca.crt'") {
		t.Errorf("certificate path not quoted: %s", c.connString())
	}
}

func TestDBConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	credentialsDir = dir
	t.Cleanup(func() { credentialsDir = CredentialsDir })
	if err := os.WriteFile(filepath.Join(dir, "username"), []byte("zabbix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("p@ss!word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHost, "pg-rw")
	t.Setenv(EnvName, "zabbix")
	t.Setenv(EnvPort, "6432")
	c, err := DBConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "zabbix" || c.Password != "p@ss!word" || c.Port != 6432 {
		t.Errorf("config %+v", c)
	}
	t.Setenv(EnvPort, "x")
	if _, err := DBConfigFromEnv(); err == nil {
		t.Error("invalid port accepted")
	}
}

func TestMainUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {CommandPrecheck, "--target-version=bad"}} {
		if code := Main(args); code != 2 {
			t.Errorf("Main(%v) = %d, want 2", args, code)
		}
	}
}

func TestResultRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result")
	in := Result{Command: CommandHAGC, OK: true, Message: "deleted 2 stale ha_node rows", Deleted: 2}
	if err := in.write(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseResult(string(b))
	if err != nil || out != in {
		t.Errorf("round trip %+v, %v", out, err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, ,b,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %q", got)
	}
}
