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

package flap

import (
	"math/rand"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	pg1 = "pg-1"
	pg2 = "pg-2"
)

func tp(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

func TestEvaluate_Table(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	window := 60 * time.Second
	const threshold = 2

	cases := []struct {
		name         string
		prevPrimary  string
		currPrimary  string
		prevCount    int
		lastChange   *metav1.Time
		now          time.Time
		wantCount    int
		wantFlapping bool
		wantChanged  bool
	}{
		{
			name:        "first observation, no prior primary",
			prevPrimary: "", currPrimary: "pg-1", prevCount: 0, lastChange: nil,
			now: base, wantCount: 0, wantFlapping: false, wantChanged: false,
		},
		{
			name:        "first real change starts window at 1",
			prevPrimary: "pg-1", currPrimary: "pg-2", prevCount: 0, lastChange: nil,
			now: base, wantCount: 1, wantFlapping: false, wantChanged: true,
		},
		{
			name:        "second change inside window trips threshold",
			prevPrimary: "pg-2", currPrimary: "pg-1", prevCount: 1, lastChange: tp(base),
			now: base.Add(10 * time.Second), wantCount: 2, wantFlapping: true, wantChanged: true,
		},
		{
			name:        "change after window elapses restarts at 1",
			prevPrimary: "pg-1", currPrimary: "pg-2", prevCount: 5, lastChange: tp(base),
			now: base.Add(90 * time.Second), wantCount: 1, wantFlapping: false, wantChanged: true,
		},
		{
			name:        "stable within window keeps count",
			prevPrimary: "pg-2", currPrimary: "pg-2", prevCount: 2, lastChange: tp(base),
			now: base.Add(30 * time.Second), wantCount: 2, wantFlapping: true, wantChanged: false,
		},
		{
			name:        "stable past window decays to zero",
			prevPrimary: "pg-2", currPrimary: "pg-2", prevCount: 5, lastChange: tp(base),
			now: base.Add(90 * time.Second), wantCount: 0, wantFlapping: false, wantChanged: false,
		},
		{
			name:        "empty previous primary never counts as a change",
			prevPrimary: "", currPrimary: "pg-1", prevCount: 0, lastChange: nil,
			now: base, wantCount: 0, wantFlapping: false, wantChanged: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.prevPrimary, tc.currPrimary, tc.prevCount, tc.lastChange, tc.now, window, threshold)
			if got.Count != tc.wantCount {
				t.Errorf("Count = %d, want %d", got.Count, tc.wantCount)
			}
			if got.Flapping != tc.wantFlapping {
				t.Errorf("Flapping = %v, want %v", got.Flapping, tc.wantFlapping)
			}
			if got.PrimaryChanged != tc.wantChanged {
				t.Errorf("PrimaryChanged = %v, want %v", got.PrimaryChanged, tc.wantChanged)
			}
		})
	}
}

// TestEvaluate_RecoveryInvariant checks that for any sequence of switchovers, once the
// cluster stays quiet for longer than the window the count returns to 0 and Flapping is
// false.
func TestEvaluate_RecoveryInvariant(t *testing.T) {
	window := 60 * time.Second
	const threshold = 2
	rng := rand.New(rand.NewSource(1))

	for iter := 0; iter < 500; iter++ {
		now := time.Unix(0, 0).UTC()
		var lastChange *metav1.Time
		count := 0
		prev := pg1

		// Random burst of switchovers within a short span.
		steps := rng.Intn(8) + 1
		for i := 0; i < steps; i++ {
			now = now.Add(time.Duration(rng.Intn(20)) * time.Second)
			curr := prev
			if rng.Intn(2) == 0 {
				if prev == pg1 {
					curr = pg2
				} else {
					curr = pg1
				}
			}
			r := Evaluate(prev, curr, count, lastChange, now, window, threshold)
			count = r.Count
			lastChange = r.LastChange
			prev = curr
		}

		// Now the cluster settles: many stable observations well past the window.
		settled := false
		for i := 0; i < 5; i++ {
			now = now.Add(2 * window)
			r := Evaluate(prev, prev, count, lastChange, now, window, threshold)
			count = r.Count
			lastChange = r.LastChange
			if !r.Flapping && r.Count == 0 {
				settled = true
				break
			}
		}
		if !settled {
			t.Fatalf("iter %d: flap count never recovered to 0 after settling (count=%d)", iter, count)
		}
	}
}
