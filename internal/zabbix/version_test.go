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

import "testing"

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"7.0.1":    {7, 0, 1, ""},
		"7.0.25":   {7, 0, 25, ""},
		"8.0.0rc1": {8, 0, 0, "rc1"},
		"8.0.1":    {8, 0, 1, ""},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, in := range []string{"7.0", "v7.0.1", "7.0.1-rc1", "", "7.0.x"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) accepted", in)
		}
	}
}

// The schema levels match the dbversion rows shipped in the official images:
// 7.0.1 inserts 7000000 (optional 7000002), 7.0.25 7000000 (optional 7000030) and
// 8.0.0rc1 7050195.
func TestSchemaLevels(t *testing.T) {
	for v, want := range map[string]int{"7.0.1": 700, "7.0.25": 700, "8.0.0rc1": 705, "8.0.0": 800, "8.0.3": 800} {
		got, _ := ParseVersion(v)
		if got.SchemaLevel() != want {
			t.Errorf("%s: level %d, want %d", v, got.SchemaLevel(), want)
		}
	}
	for mandatory, want := range map[int]int{7000000: 700, 7020000: 702, 7040000: 704, 7050195: 705, 8000000: 800} {
		if got := SchemaLevelOf(mandatory); got != want {
			t.Errorf("SchemaLevelOf(%d) = %d, want %d", mandatory, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	v := func(s string) Version { r, _ := ParseVersion(s); return r }
	cases := []struct {
		from   int
		target string
		want   Change
	}{
		{0, "7.0.1", FreshInstall},
		{700, "7.0.25", SameSchema}, // 7.0.1 → 7.0.25: optional patches only, rolled
		{700, "8.0.0rc1", SchemaUpgrade},
		{700, "8.0.0", SchemaUpgrade},
		{704, "8.0.0", SchemaUpgrade}, // a 7.4 database can move to 8.0
		{704, "8.0.0rc1", SchemaUpgrade},
		{704, "7.4.7", SameSchema},
		{700, "7.2.15", SchemaUpgrade},
		{702, "7.4.0", SchemaUpgrade},
		{700, "7.4.7", SchemaUpgrade},
		{703, "7.4.0rc1", SameSchema}, // 7.4 release candidates run the 7.3 series
		{703, "7.4.0", SchemaUpgrade},
		{704, "7.2.15", Downgrade},
		{705, "8.0.0rc1", SameSchema},
		{705, "8.0.0", SchemaUpgrade}, // release candidate to final release
		{800, "8.0.3", SameSchema},
		{704, "7.0.25", Downgrade},
		{705, "7.0.25", Downgrade},
		{800, "8.0.0rc1", Downgrade},
	}
	for _, c := range cases {
		if got := Classify(c.from, v(c.target)); got != c.want {
			t.Errorf("Classify(%d, %s) = %s, want %s", c.from, c.target, got, c.want)
		}
	}
}

func TestUnsupportedMessage(t *testing.T) {
	v, _ := ParseVersion("9.0.0")
	want := "Zabbix 9.0 is not supported by this operator version (supported lines: 7.0, 7.2, 7.4, 8.0)"
	if got := UnsupportedMessage(v); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPostgresRequirements(t *testing.T) {
	v := func(s string) Version { r, _ := ParseVersion(s); return r }
	for _, s := range []string{"7.0.1", "7.2.15", "7.4.7", "8.0.0rc1"} {
		if !v(s).Supported() {
			t.Errorf("%s must be supported", s)
		}
	}
	for _, s := range []string{"6.0.40", "7.1.0", "7.3.0", "9.0.0"} {
		if v(s).Supported() {
			t.Errorf("%s must not be supported", s)
		}
	}
	if v("7.0.25").MinPostgres() != 13 || v("7.4.7").MinPostgres() != 13 || v("8.0.0rc1").MinPostgres() != 15 {
		t.Errorf("minimums: 7.0 %d, 8.0 %d", v("7.0.25").MinPostgres(), v("8.0.0rc1").MinPostgres())
	}
}

func TestCompare(t *testing.T) {
	ordered := []string{"7.0.1", "7.0.25", "7.0.30", "7.2.15", "7.4.0rc1", "7.4.0", "7.4.7", "8.0.0alpha2", "8.0.0beta1", "8.0.0rc1", "8.0.0rc2", "8.0.0", "8.0.1"}
	for i := range ordered {
		for j := range ordered {
			a, _ := ParseVersion(ordered[i])
			b, _ := ParseVersion(ordered[j])
			want := sign(i - j)
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}
