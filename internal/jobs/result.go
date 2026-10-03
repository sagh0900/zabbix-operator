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

// Package jobs implements the short-lived Jobs the operator runs against the Zabbix
// database (precheck, ha-reset, ha-gc) and builds their Kubernetes Job objects. The
// commands run inside the operator image as "manager job <command>".
package jobs

import (
	"encoding/json"
	"os"
)

// Result is what a Job reports. It is written to the container's termination message, so
// the operator reads it from the pod status without parsing logs.
type Result struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	// Reason is a CamelCase cause when OK is false, for example PostgreSQLTooOld.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`

	// precheck
	PostgresMajor int    `json:"postgresMajor,omitempty"`
	SchemaLevel   int    `json:"schemaLevel,omitempty"`
	Change        string `json:"change,omitempty"`
	ActiveNodes   int    `json:"activeNodes,omitempty"`

	// ha-reset and ha-gc
	Deleted int64 `json:"deleted,omitempty"`
}

// write stores r at path (the termination message file) and prints it.
func (r Result) write(path string) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	os.Stdout.Write(append(b, '\n')) //nolint:errcheck // best-effort log copy
	if path == "" {
		return nil
	}
	return os.WriteFile(path, b, 0o644)
}

// Retryable reports whether the command failed for a reason that may go away by itself,
// such as a database that cannot be reached, rather than reporting a finding.
func (r Result) Retryable() bool {
	switch r.Reason {
	case reasonDatabaseUnreachable, "Configuration", reasonDatabaseError:
		return true
	}
	return false
}

// ParseResult decodes a termination message written by a Job.
func ParseResult(message string) (Result, error) {
	var r Result
	err := json.Unmarshal([]byte(message), &r)
	return r, err
}
