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
	"strconv"
)

// Version is a Zabbix release such as 7.0.25 or 8.0.0rc1.
type Version struct {
	Major, Minor, Patch int
	// Pre is a pre-release suffix such as rc1; empty for a final release.
	Pre string
}

var preRe = regexp.MustCompile(`^(alpha|beta|rc)(\d+)$`)

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

// Compare returns -1, 0 or 1 when v is older than, equal to or newer than o. A
// pre-release is older than the final release of the same number (8.0.0rc1 < 8.0.0), and
// alpha < beta < rc.
func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	return sign(preRank(v.Pre) - preRank(o.Pre))
}

// preRank orders pre-releases below the final release; within a kind by number.
func preRank(pre string) int {
	if pre == "" {
		return 1 << 30
	}
	m := preRe.FindStringSubmatch(pre)
	n, _ := strconv.Atoi(m[2])
	return map[string]int{"alpha": 1, "beta": 2, "rc": 3}[m[1]]<<20 + n
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// Line is the release line, for example "8.0".
func (v Version) Line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// SchemaLevel is the database schema level this version runs, in the units of
// SchemaLevelOf: 700 for 7.0, 704 for 7.4, 800 for 8.0. Pre-releases of a line run the
// schema of the development series before it: 8.0.0rc1 reports 7050195 (level 705), and
// 7.4.0rc1 runs the 7.3 series (level 703).
func (v Version) SchemaLevel() int {
	switch {
	case v.Pre == "" || v.Patch != 0:
		return v.Major*100 + v.Minor
	case v.Minor == 0:
		return (v.Major-1)*100 + 5
	default:
		return v.Major*100 + v.Minor - 1
	}
}

// SchemaLevelOf converts the dbversion.mandatory value of a Zabbix database to a
// schema level: 7000000 → 700, 7040000 → 704, 7050195 → 705, 8000000 → 800.
func SchemaLevelOf(mandatory int) int { return mandatory / 10000 }

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
