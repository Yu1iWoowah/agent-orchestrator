package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/source"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

func runCurrent(ctx context.Context, id string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client, err := source.FromEnvironment()
	snapshot := usage.Snapshot{Source: "daemon_ledger", ScanStatus: "unavailable", Page: usage.Page{SessionID: id}}
	if err == nil {
		snapshot, err = client.Scan(ctx, id)
	} else {
		snapshot.Reason = err.Error()
	}
	return writeUsageResult(usage.Current(snapshot), err, stdout, stderr)
}

func runBaseline(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("usage baseline", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ids := flags.String("sessions", "", "explicit comma-separated AO session ids")
	from := flags.String("from", "", "inclusive UTC RFC3339 timestamp")
	to := flags.String("to", "", "exclusive UTC RFC3339 timestamp")
	flags.Bool("json", false, "emit JSON (also the default)")
	if flags.Parse(args) != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	query, err := usage.NewCohort(strings.Split(*ids, ","), *from, *to)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client, err := source.FromEnvironment()
	snapshots := make([]usage.Snapshot, 0, len(query.SessionIDs))
	var failures error
	for _, id := range query.SessionIDs {
		snapshot := usage.Snapshot{Source: "daemon_ledger", ScanStatus: "unavailable", Page: usage.Page{SessionID: id}}
		if err == nil {
			var scanErr error
			snapshot, scanErr = client.Scan(ctx, id)
			if scanErr != nil {
				failures = errors.Join(failures, fmt.Errorf("%s: %w", id, scanErr))
			}
		} else {
			snapshot.Reason = err.Error()
		}
		snapshots = append(snapshots, snapshot)
	}
	report, reportErr := usage.Baseline(query, snapshots)
	return writeUsageResult(report, errors.Join(err, failures, reportErr), stdout, stderr)
}

func writeUsageResult(value any, err error, stdout, stderr io.Writer) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(value); encodeErr != nil {
		fmt.Fprintln(stderr, encodeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
