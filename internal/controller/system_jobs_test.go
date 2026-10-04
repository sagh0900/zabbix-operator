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

package controller

import (
	"testing"
	"time"

	"github.com/sagh0900/zabbix-operator/internal/jobs"
)

func TestRemoveFinishedJob(t *testing.T) {
	ok := &jobs.Result{OK: true}
	failed := &jobs.Result{Reason: "DatabaseError"}
	for _, c := range []struct {
		name    string
		cleanup jobCleanup
		res     *jobs.Result
		age     time.Duration
		want    bool
	}{
		{"no result yet", keepFinished, nil, time.Hour, false},
		{"periodic success is left to the caller", keepFinished, ok, time.Hour, false},
		{"periodic failure is kept", keepFinished, failed, time.Hour, false},
		{"success answers repeated questions", retryFailed, ok, time.Hour, false},
		{"fresh failure stays for a while", retryFailed, failed, 30 * time.Second, false},
		{"old failure is removed to retry", retryFailed, failed, 2 * time.Minute, true},
	} {
		if got := removeFinishedJob(c.cleanup, c.res, c.age); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
