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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// Commands the Job runner accepts.
const (
	CommandPrecheck = "precheck"
	CommandHAReset  = "ha-reset"
	CommandHAGC     = "ha-gc"
)

// Main runs "manager job <command> [flags]" and returns the process exit code: 0 when
// the result is OK, 1 when the command ran and reported a problem, 2 on usage errors.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: manager job precheck|ha-reset|ha-gc [flags]")
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	resultFile := fs.String("result-file", "/dev/termination-log", "Where to write the JSON result.")
	target := fs.String("target-version", "", "precheck: the Zabbix version to check for.")
	keep := fs.String("keep", "", "ha-gc: comma-separated names of live server pods.")
	stale := fs.Int("stale-seconds", 30, "ha-reset, ha-gc: rows heartbeating within this window are live.")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var r Result
	switch cmd {
	case CommandPrecheck, CommandHAReset, CommandHAGC:
	default:
		fmt.Fprintf(os.Stderr, "unknown job command %q\n", cmd)
		return 2
	}
	var v zabbix.Version
	if cmd == CommandPrecheck {
		var err error
		if v, err = zabbix.ParseVersion(*target); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	cfg, err := DBConfigFromEnv()
	if err != nil {
		r = Result{Command: cmd, Reason: "Configuration", Message: err.Error()}
	} else if conn, err := Connect(ctx, cfg); err != nil {
		r = Result{Command: cmd, Reason: "DatabaseUnreachable", Message: err.Error()}
	} else {
		defer conn.Close(context.Background()) //nolint:errcheck // process exits next
		switch cmd {
		case CommandPrecheck:
			r = Precheck(ctx, conn, v)
		case CommandHAReset:
			r = HAReset(ctx, conn, *stale)
		case CommandHAGC:
			r = HAGC(ctx, conn, splitList(*keep), *stale)
		}
	}
	if err := r.write(*resultFile); err != nil {
		fmt.Fprintln(os.Stderr, "writing result:", err)
	}
	if !r.OK {
		return 1
	}
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
