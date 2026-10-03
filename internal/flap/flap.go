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

// Package flap damps CNPG primary changes so a burst of switchovers is reported as
// unstable, and a cluster that has been quiet for a full window is reported as stable.
package flap

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Defaults applied when the caller passes a non-positive window or threshold.
const (
	DefaultWindow    = 10 * time.Minute
	DefaultThreshold = 3
)

// Result is the outcome of one evaluation.
type Result struct {
	// Count is the number of primary changes in the current window.
	Count int
	// Flapping is true when Count has reached the threshold.
	Flapping bool
	// PrimaryChanged is true when this evaluation observed a new primary.
	PrimaryChanged bool
	// LastChange is the time of the most recent primary change.
	LastChange *metav1.Time
}

// Evaluate compares the previously recorded primary with the current one.
//
//   - A primary change inside the window increments the count; a change after the
//     window has elapsed starts a new window at 1.
//   - Without a change, the count drops to 0 once the last change is older than the window.
//   - Flapping is true when the count reaches threshold.
//
// An empty primary on either side is not a change, so a cluster coming up for the first
// time never counts as flapping.
func Evaluate(
	prevPrimary, currentPrimary string,
	prevCount int,
	lastChange *metav1.Time,
	now time.Time,
	window time.Duration,
	threshold int,
) Result {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if window <= 0 {
		window = DefaultWindow
	}

	res := Result{Count: prevCount, LastChange: lastChange}
	windowElapsed := lastChange == nil || now.Sub(lastChange.Time) > window
	res.PrimaryChanged = prevPrimary != "" && currentPrimary != "" && currentPrimary != prevPrimary

	switch {
	case res.PrimaryChanged && windowElapsed:
		res.Count = 1
		stamp := metav1.NewTime(now)
		res.LastChange = &stamp
	case res.PrimaryChanged:
		res.Count = prevCount + 1
		stamp := metav1.NewTime(now)
		res.LastChange = &stamp
	case prevCount > 0 && windowElapsed:
		res.Count = 0
	}

	res.Flapping = res.Count >= threshold
	return res
}
