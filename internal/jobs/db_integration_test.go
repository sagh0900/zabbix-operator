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
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// The database tests need a PostgreSQL server: ZO_TEST_PG holds a superuser connection
// string. "make test-db" starts one per version in PG_VERSIONS and sets ZO_REQUIRE_DB so
// a missing server fails instead of skipping.

// zabbixSchema is the ha_node and dbversion DDL shipped by the official Zabbix 7.0 and
// 8.0 images (identical in 7.0.1, 7.0.25 and 8.0.0rc1).
const zabbixSchema = `
CREATE TABLE ha_node (
	ha_nodeid                varchar(25)                               NOT NULL,
	name                     varchar(255)    DEFAULT ''                NOT NULL,
	address                  varchar(255)    DEFAULT ''                NOT NULL,
	port                     integer         DEFAULT '10051'           NOT NULL,
	lastaccess               integer         DEFAULT '0'               NOT NULL,
	status                   integer         DEFAULT '0'               NOT NULL,
	ha_sessionid             varchar(25)     DEFAULT ''                NOT NULL,
	PRIMARY KEY (ha_nodeid)
);
CREATE UNIQUE INDEX ha_node_1 ON ha_node (name);
CREATE INDEX ha_node_2 ON ha_node (status,lastaccess);
CREATE TABLE dbversion (
	dbversionid              bigint                                    NOT NULL,
	mandatory                integer         DEFAULT '0'               NOT NULL,
	optional                 integer         DEFAULT '0'               NOT NULL,
	PRIMARY KEY (dbversionid)
);`

type testDB struct {
	t    *testing.T
	conn *pgx.Conn
	cfg  DBConfig
	pg   int // PostgreSQL major version
}

// newTestDB creates an empty database for one test and drops it afterwards.
func newTestDB(t *testing.T) *testDB {
	t.Helper()
	dsn := os.Getenv("ZO_TEST_PG")
	if dsn == "" {
		if os.Getenv("ZO_REQUIRE_DB") != "" {
			t.Fatal("ZO_REQUIRE_DB is set but ZO_TEST_PG is empty")
		}
		t.Skip("ZO_TEST_PG not set; run make test-db")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	var num int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})

	pc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DBConfig{Host: pc.Host, Port: int(pc.Port), Name: name, User: pc.User, Password: pc.Password, SSLMode: "disable"}
	conn, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return &testDB{t: t, conn: conn, cfg: cfg, pg: num / 10000}
}

func (d *testDB) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.conn.Exec(context.Background(), sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

// schema loads the Zabbix tables with the given dbversion.mandatory.
func (d *testDB) schema(mandatory int) {
	d.t.Helper()
	d.exec(zabbixSchema)
	d.exec("INSERT INTO dbversion VALUES (1, $1, $1)", mandatory)
}

// node inserts an ha_node row whose last heartbeat was ago seconds before now.
func (d *testDB) node(name string, status, ago int) {
	d.t.Helper()
	d.exec("INSERT INTO ha_node (ha_nodeid, name, status, lastaccess) VALUES ($1, $1, $2, extract(epoch FROM now())::int - $3)",
		name, status, ago)
}

func (d *testDB) nodes() []string {
	d.t.Helper()
	rows, err := d.conn.Query(context.Background(), "SELECT name FROM ha_node ORDER BY name")
	if err != nil {
		d.t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		d.t.Fatal(err)
	}
	return names
}

func version(t *testing.T, s string) zabbix.Version {
	t.Helper()
	v, err := zabbix.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDBPrecheckFreshInstall(t *testing.T) {
	d := newTestDB(t)
	r := Precheck(context.Background(), d.conn, version(t, "7.0.1"))
	if !r.OK || r.Change != string(zabbix.FreshInstall) || r.PostgresMajor != d.pg {
		t.Fatalf("result %+v", r)
	}
}

func TestDBPrecheckSameSchemaCountsActiveNodes(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	d.node("zabbix-server-0", haNodeActive, 1)
	d.node("zabbix-server-1", 0, 1) // standby
	r := Precheck(context.Background(), d.conn, version(t, "7.0.25"))
	if !r.OK || r.Change != string(zabbix.SameSchema) || r.SchemaLevel != 700 || r.ActiveNodes != 1 {
		t.Fatalf("result %+v", r)
	}
}

// Upgrading to 8.0 needs PostgreSQL 15: on 14 it is blocked with a clear reason, on newer
// versions it is a schema upgrade.
func TestDBPrecheckMajorUpgradeHonoursPostgresMinimum(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	r := Precheck(context.Background(), d.conn, version(t, "8.0.0rc1"))
	if d.pg < 15 {
		want := "PostgreSQL " + strconv.Itoa(d.pg) + " is too old for Zabbix 8.0 (needs 15 or newer)"
		if r.OK || r.Reason != "PostgreSQLTooOld" || r.Message != want {
			t.Fatalf("result %+v, want PostgreSQLTooOld %q", r, want)
		}
		return
	}
	if !r.OK || r.Change != string(zabbix.SchemaUpgrade) {
		t.Fatalf("result %+v", r)
	}
}

func TestDBPrecheckRefusesDowngrade(t *testing.T) {
	d := newTestDB(t)
	d.schema(7050195) // an 8.0.0rc1 database
	r := Precheck(context.Background(), d.conn, version(t, "7.0.25"))
	if r.OK || r.Reason != "Downgrade" || !strings.Contains(r.Message, "7050195") {
		t.Fatalf("result %+v", r)
	}
}

func TestDBPrecheckUnsupportedLine(t *testing.T) {
	d := newTestDB(t)
	r := Precheck(context.Background(), d.conn, version(t, "9.0.0"))
	if r.OK || r.Reason != "UnsupportedVersion" || !strings.Contains(r.Message, "not supported by this operator version") {
		t.Fatalf("result %+v", r)
	}
}

func TestDBHAResetRefusesWhileNodesHeartbeat(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	d.node("old-0", 1, 600)
	d.node("zabbix-server-0", haNodeActive, 2)
	r := HAReset(context.Background(), d.conn, 30)
	if r.OK || r.Reason != "LiveNodes" || !strings.Contains(r.Message, "zabbix-server-0") {
		t.Fatalf("result %+v", r)
	}
	if len(d.nodes()) != 2 {
		t.Fatalf("rows deleted despite live nodes: %v", d.nodes())
	}
}

func TestDBHAResetClearsStaleTable(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	d.node("zabbix-server-0", haNodeActive, 120) // crashed active node
	d.node("zabbix-server-1", 0, 120)
	r := HAReset(context.Background(), d.conn, 30)
	if !r.OK || r.Deleted != 2 || len(d.nodes()) != 0 {
		t.Fatalf("result %+v, rows %v", r, d.nodes())
	}
}

// ha-gc removes only rows that are neither live pods nor heartbeating.
func TestDBHAGCKeepsLiveAndHeartbeatingNodes(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	d.node("zabbix-server-0", haNodeActive, 1) // live pod
	d.node("zabbix-server-1", 0, 300)          // live pod, stale heartbeat (just restarted)
	d.node("zabbix-server-2", 0, 2)            // not in keep but heartbeating (being replaced)
	d.node("zabbix-server-7c9f4-abcde", 1, 900)
	d.node("zabbix-server-7c9f4-fghij", 2, 900)
	r := HAGC(context.Background(), d.conn, []string{"zabbix-server-0", "zabbix-server-1"}, 30)
	if !r.OK || r.Deleted != 2 {
		t.Fatalf("result %+v", r)
	}
	if got := strings.Join(d.nodes(), ","); got != "zabbix-server-0,zabbix-server-1,zabbix-server-2" {
		t.Fatalf("rows left %s", got)
	}
}

func TestDBHACommandsOnEmptyDatabase(t *testing.T) {
	d := newTestDB(t)
	if r := HAReset(context.Background(), d.conn, 30); !r.OK {
		t.Errorf("ha-reset: %+v", r)
	}
	if r := HAGC(context.Background(), d.conn, nil, 30); !r.OK {
		t.Errorf("ha-gc: %+v", r)
	}
}

// Passwords with characters that break hand-built connection strings work.
func TestDBSpecialCharacterPassword(t *testing.T) {
	d := newTestDB(t)
	password := `p!@#$%^&*()'"\ x=y`
	d.exec("DROP ROLE IF EXISTS zbx_special")
	d.exec("CREATE ROLE zbx_special LOGIN PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "'")
	t.Cleanup(func() { _, _ = d.conn.Exec(context.Background(), "DROP OWNED BY zbx_special; DROP ROLE zbx_special") })
	cfg := d.cfg
	cfg.User, cfg.Password = "zbx_special", password
	conn, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting with special-character password: %v", err)
	}
	_ = conn.Close(context.Background())
}

// Main reads its configuration the way a Job pod provides it and writes the result.
func TestDBMainEndToEnd(t *testing.T) {
	d := newTestDB(t)
	d.schema(7000000)
	dir := t.TempDir()
	credentialsDir = dir
	t.Cleanup(func() { credentialsDir = CredentialsDir })
	for f, v := range map[string]string{"username": d.cfg.User, "password": d.cfg.Password} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvHost, d.cfg.Host)
	t.Setenv(EnvPort, strconv.Itoa(d.cfg.Port))
	t.Setenv(EnvName, d.cfg.Name)
	t.Setenv(EnvSSLMode, "disable")
	resultFile := filepath.Join(dir, "result")

	if code := Main([]string{CommandPrecheck, "--target-version=7.0.25", "--result-file=" + resultFile}); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	b, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseResult(string(b))
	if err != nil || !r.OK || r.Change != string(zabbix.SameSchema) {
		t.Fatalf("result %+v, %v", r, err)
	}
	d.node("zabbix-server-0", haNodeActive, 1)
	if code := Main([]string{CommandHAReset, "--result-file=" + resultFile}); code != 1 {
		t.Fatalf("ha-reset with a live node: exit code %d, want 1", code)
	}
}
