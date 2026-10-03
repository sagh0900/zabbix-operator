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

// Package zabbix holds facts about Zabbix versions, database schemas and their
// PostgreSQL requirements.
package zabbix

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Version is a Zabbix release such as 7.0.25 or 8.0.0rc1.
type Version struct {
	Major, Minor, Patch int
	// Pre is a pre-release suffix such as rc1; empty for a final release.
	Pre string
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)((?:alpha|beta|rc)\d+)?$`)

// ParseVersion parses a Zabbix version.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("invalid Zabbix version %q", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch, Pre: m[4]}, nil
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d%s", v.Major, v.Minor, v.Patch, v.Pre)
}

// Line is the release line, for example "8.0".
func (v Version) Line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// SchemaLevel is the database schema level this version runs, in the units of
// SchemaLevelOf: 700 for 7.0, 800 for 8.0. Pre-releases of an LTS run the schema of the
// development series before it (8.0.0rc1 reports 7050195, level 705).
func (v Version) SchemaLevel() int {
	level := v.Major*100 + v.Minor
	if v.Pre != "" && v.Minor == 0 {
		return (v.Major-1)*100 + 5
	}
	return level
}

// SchemaLevelOf converts the dbversion.mandatory value of a Zabbix database to a
// schema level: 7000000 → 700, 7040000 → 704, 7050195 → 705, 8000000 → 800.
func SchemaLevelOf(mandatory int) int { return mandatory / 10000 }

// supportedLines are the release lines this operator runs, with the minimum
// PostgreSQL major version each requires.
var supportedLines = map[string]int{
	"7.0": 13,
	"8.0": 15,
}

// SupportedLines lists the release lines this operator runs, oldest first.
func SupportedLines() []string {
	lines := make([]string, 0, len(supportedLines))
	for l := range supportedLines {
		lines = append(lines, l)
	}
	sort.Slice(lines, func(i, j int) bool {
		a, _ := ParseVersion(lines[i] + ".0")
		b, _ := ParseVersion(lines[j] + ".0")
		return a.SchemaLevel() < b.SchemaLevel()
	})
	return lines
}

// UnsupportedMessage explains that v's release line is not supported.
func UnsupportedMessage(v Version) string {
	return fmt.Sprintf("Zabbix %s is not supported by this operator version (supported lines: %s)",
		v.Line(), strings.Join(SupportedLines(), ", "))
}

// Supported reports whether the operator runs this version's release line.
func (v Version) Supported() bool {
	_, ok := supportedLines[v.Line()]
	return ok
}

// MinPostgres is the minimum PostgreSQL major version the release line requires.
func (v Version) MinPostgres() int { return supportedLines[v.Line()] }

// Change classifies moving a database to a target version.
type Change string

// Changes a database can undergo.
const (
	// FreshInstall: the database has no Zabbix schema yet.
	FreshInstall Change = "FreshInstall"
	// SameSchema: the schema already matches the target; servers can be rolled one at a time.
	SameSchema Change = "SameSchema"
	// SchemaUpgrade: the target upgrades the schema; all servers must stop first.
	SchemaUpgrade Change = "SchemaUpgrade"
	// Downgrade: the schema is newer than the target; Zabbix cannot run on it.
	Downgrade Change = "Downgrade"
)

// Classify decides what moving a database with schema level from (0: no schema) to
// target involves. Only dbversion.mandatory decides compatibility: patch releases of a line
// share it and differ only in optional patches (7.0.1 ships 7000000/7000002, 7.0.25
// ships 7000000/7000030), which a newer server applies while it runs, so servers of the
// same schema level can be rolled one at a time.
func Classify(from int, target Version) Change {
	to := target.SchemaLevel()
	switch {
	case from == 0:
		return FreshInstall
	case from == to:
		return SameSchema
	case from < to:
		return SchemaUpgrade
	default:
		return Downgrade
	}
}
