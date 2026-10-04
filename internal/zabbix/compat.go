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
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Line describes a release line the operator runs.
type Line struct {
	// Line is the release line, for example "8.0".
	Line string `json:"line"`
	// MinPostgres and MaxPostgres bound the PostgreSQL major version; 0 means unbounded.
	MinPostgres int `json:"minPostgres"`
	MaxPostgres int `json:"maxPostgres,omitempty"`
	// Verified is true for lines validated with this operator version; lines added or
	// changed through configuration are not.
	Verified bool `json:"-"`
}

// builtinLines are the release lines validated with this operator version, with the
// PostgreSQL range of each line's official requirements (the 7.2 documentation no longer
// states an upper bound).
var builtinLines = []Line{
	{Line: "7.0", MinPostgres: 13, MaxPostgres: 18, Verified: true},
	{Line: "7.2", MinPostgres: 13, Verified: true},
	{Line: "7.4", MinPostgres: 13, MaxPostgres: 18, Verified: true},
	{Line: "8.0", MinPostgres: 15, MaxPostgres: 18, Verified: true},
}

// Compatibility is the set of release lines the operator runs. Every line runs, and every
// newer line can be upgraded to, through a schema upgrade.
type Compatibility struct {
	lines map[string]Line
}

// Builtin returns the release lines validated with this operator version.
func Builtin() *Compatibility {
	c := &Compatibility{lines: map[string]Line{}}
	for _, l := range builtinLines {
		c.lines[l.Line] = l
	}
	return c
}

var lineRe = regexp.MustCompile(`^[1-9][0-9]?\.(0|[1-9][0-9]?)$`)

// ParseCompatibility adds the lines of a configuration document to the built-in lines.
// A configured line replaces a built-in one of the same name; it stays verified only when
// its limits are unchanged.
//
//	lines:
//	  - line: "8.2"
//	    minPostgres: 15
//	    maxPostgres: 18
func ParseCompatibility(data []byte) (*Compatibility, error) {
	var doc struct {
		Lines []Line `json:"lines"`
	}
	if err := yaml.UnmarshalStrict(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing the compatibility configuration: %w", err)
	}
	c := Builtin()
	seen := map[string]bool{}
	for _, l := range doc.Lines {
		switch {
		case !lineRe.MatchString(l.Line):
			return nil, fmt.Errorf("line %q: must look like 8.0", l.Line)
		case seen[l.Line]:
			return nil, fmt.Errorf("line %s is listed more than once", l.Line)
		case l.MinPostgres < 10:
			return nil, fmt.Errorf("line %s: minPostgres must be 10 or newer", l.Line)
		case l.MaxPostgres != 0 && l.MaxPostgres < l.MinPostgres:
			return nil, fmt.Errorf("line %s: maxPostgres is lower than minPostgres", l.Line)
		}
		seen[l.Line] = true
		b, builtin := c.lines[l.Line]
		l.Verified = builtin && b.MinPostgres == l.MinPostgres && b.MaxPostgres == l.MaxPostgres
		c.lines[l.Line] = l
	}
	return c, nil
}

// Lookup returns the line of v and whether it is supported.
func (c *Compatibility) Lookup(v Version) (Line, bool) {
	l, ok := c.lines[v.Line()]
	return l, ok
}

// Lines lists the supported lines, oldest first.
func (c *Compatibility) Lines() []string {
	out := make([]string, 0, len(c.lines))
	for l := range c.lines {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := ParseVersion(out[i] + ".0")
		b, _ := ParseVersion(out[j] + ".0")
		return a.Compare(b) < 0
	})
	return out
}

// UnsupportedMessage explains that v's release line is not supported.
func (c *Compatibility) UnsupportedMessage(v Version) string {
	return fmt.Sprintf("Zabbix %s is not supported by this operator version (supported lines: %s)",
		v.Line(), strings.Join(c.Lines(), ", "))
}

// PostgresProblem returns why PostgreSQL major version pg cannot run line l, or "".
func (l Line) PostgresProblem(pg int) string {
	switch {
	case l.MinPostgres > 0 && pg < l.MinPostgres:
		return fmt.Sprintf("PostgreSQL %d is too old for Zabbix %s (needs %d or newer)", pg, l.Line, l.MinPostgres)
	case l.MaxPostgres > 0 && pg > l.MaxPostgres:
		return fmt.Sprintf("PostgreSQL %d is too new for Zabbix %s (supports up to %d)", pg, l.Line, l.MaxPostgres)
	}
	return ""
}
