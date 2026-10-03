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

// Package version holds build metadata stamped in with -ldflags -X at build time.
package version

import (
	"fmt"
	"runtime"
)

// Build metadata. The Makefile and Dockerfile override these with -ldflags -X.
var (
	Version   = "0.0.0-dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	return fmt.Sprintf("v%s (commit %s, built %s, %s)", Version, GitCommit, BuildDate, runtime.Version())
}
