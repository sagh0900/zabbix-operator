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
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// Zabbix HA node status as stored in ha_node.status: 0 standby, 1 stopped, 2 unavailable,
// 3 active.
const (
	haNodeStandby = 0
	haNodeActive  = 3
)

// Reasons for failures worth retrying.
const (
	reasonDatabaseError       = "DatabaseError"
	reasonDatabaseUnreachable = "DatabaseUnreachable"
)

// Precheck verifies the database can take target: it is reachable, is the primary, runs a
// PostgreSQL version the target line supports, and holds a schema the target can run on
// or upgrade. It also counts active HA nodes, so callers can tell whether servers still
// run against the database.
func Precheck(ctx context.Context, conn *pgx.Conn, target zabbix.Version) Result {
	var f facts
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int, pg_is_in_recovery()").
		Scan(&f.versionNum, &f.inRecovery); err != nil {
		return Result{Command: CommandPrecheck, Reason: reasonDatabaseUnreachable, Message: fmt.Sprintf("querying the server: %v", err)}
	}
	mandatory, err := schemaMandatory(ctx, conn)
	if err != nil {
		return Result{Command: CommandPrecheck, Reason: "SchemaUnreadable", Message: fmt.Sprintf("reading dbversion: %v", err)}
	}
	f.mandatory = mandatory
	if mandatory > 0 {
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM ha_node WHERE status = $1", haNodeActive).
			Scan(&f.activeNodes); err != nil && !isUndefinedTable(err) {
			return Result{Command: CommandPrecheck, Reason: "SchemaUnreadable", Message: fmt.Sprintf("reading ha_node: %v", err)}
		}
	}
	return decide(f, target)
}

// facts are what precheck reads from the database.
type facts struct {
	versionNum  int // server_version_num
	inRecovery  bool
	mandatory   int // dbversion.mandatory; 0 without a Zabbix schema
	activeNodes int
}

// decide turns facts into a precheck result. Checks run in order of what a person must fix
// first: an unsupported target, a replica connection, an old PostgreSQL, a downgrade.
func decide(f facts, target zabbix.Version) Result {
	r := Result{Command: CommandPrecheck, PostgresMajor: f.versionNum / 10000, ActiveNodes: f.activeNodes}
	fail := func(reason, format string, args ...any) Result {
		r.Reason, r.Message = reason, fmt.Sprintf(format, args...)
		return r
	}
	if !target.Supported() {
		return fail("UnsupportedVersion", "%s", zabbix.UnsupportedMessage(target))
	}
	if f.inRecovery {
		return fail("NotPrimary", "the database is in recovery; connect to the primary")
	}
	if r.PostgresMajor < target.MinPostgres() {
		return fail("PostgreSQLTooOld", "PostgreSQL %d is too old for Zabbix %s (needs %d or newer)",
			r.PostgresMajor, target.Line(), target.MinPostgres())
	}
	if f.mandatory > 0 {
		r.SchemaLevel = zabbix.SchemaLevelOf(f.mandatory)
	}
	change := zabbix.Classify(r.SchemaLevel, target)
	r.Change = string(change)
	if change == zabbix.Downgrade {
		return fail("Downgrade", "the database schema (dbversion %d) is newer than Zabbix %s; Zabbix cannot downgrade",
			f.mandatory, target)
	}
	r.OK = true
	r.Message = fmt.Sprintf("PostgreSQL %d, %s", r.PostgresMajor, describe(change, f.mandatory, target))
	return r
}

func describe(c zabbix.Change, mandatory int, target zabbix.Version) string {
	switch c {
	case zabbix.FreshInstall:
		return "empty database: the schema will be created for Zabbix " + target.String()
	case zabbix.SameSchema:
		return fmt.Sprintf("schema %d already matches Zabbix %s", mandatory, target)
	default:
		return fmt.Sprintf("schema %d will be upgraded for Zabbix %s", mandatory, target)
	}
}

// schemaMandatory returns dbversion.mandatory, or 0 when the database has no Zabbix schema.
func schemaMandatory(ctx context.Context, conn *pgx.Conn) (int, error) {
	var mandatory int
	err := conn.QueryRow(ctx, "SELECT mandatory FROM dbversion").Scan(&mandatory)
	switch {
	case isUndefinedTable(err), errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return mandatory, nil
}

// HAReset deletes every ha_node row, so a server started next registers cleanly. It runs
// only when no server pod exists; as a safeguard it refuses while any standby or active
// node has heartbeated within staleSeconds, which means a server is still running
// somewhere. Rows of nodes that stopped cleanly or are marked unavailable do not block.
func HAReset(ctx context.Context, conn *pgx.Conn, staleSeconds int) Result {
	r := Result{Command: CommandHAReset}
	var live []string
	rows, err := conn.Query(ctx,
		"SELECT name FROM ha_node WHERE status IN ($2, $3) AND lastaccess >= extract(epoch FROM now())::int - $1 ORDER BY name",
		staleSeconds, haNodeStandby, haNodeActive)
	if err != nil {
		if isUndefinedTable(err) {
			r.OK, r.Message = true, "no ha_node table; nothing to reset"
			return r
		}
		r.Reason, r.Message = reasonDatabaseError, err.Error()
		return r
	}
	live, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.Reason, r.Message = reasonDatabaseError, err.Error()
		return r
	}
	if len(live) > 0 {
		r.Reason = "LiveNodes"
		r.Message = fmt.Sprintf("refusing to reset: %s heartbeated within %ds", strings.Join(live, ", "), staleSeconds)
		return r
	}
	tag, err := conn.Exec(ctx, "DELETE FROM ha_node")
	if err != nil {
		r.Reason, r.Message = reasonDatabaseError, err.Error()
		return r
	}
	r.OK, r.Deleted = true, tag.RowsAffected()
	r.Message = fmt.Sprintf("deleted %d ha_node rows", r.Deleted)
	return r
}

// HAGC deletes ha_node rows that belong to no live server pod and have not heartbeated
// within staleSeconds. Live pods are never touched, and a pod briefly missing from keep
// (for example while being replaced) is protected by its recent heartbeat.
func HAGC(ctx context.Context, conn *pgx.Conn, keep []string, staleSeconds int) Result {
	r := Result{Command: CommandHAGC}
	if keep == nil {
		keep = []string{}
	}
	tag, err := conn.Exec(ctx,
		"DELETE FROM ha_node WHERE name <> ALL($1) AND lastaccess < extract(epoch FROM now())::int - $2",
		keep, staleSeconds)
	if err != nil {
		if isUndefinedTable(err) {
			r.OK, r.Message = true, "no ha_node table; nothing to collect"
			return r
		}
		r.Reason, r.Message = reasonDatabaseError, err.Error()
		return r
	}
	r.OK, r.Deleted = true, tag.RowsAffected()
	r.Message = fmt.Sprintf("deleted %d stale ha_node rows", r.Deleted)
	return r
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
