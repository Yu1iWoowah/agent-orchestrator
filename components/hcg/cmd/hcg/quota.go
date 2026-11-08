package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/quota"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/source"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

func runQuota(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("usage "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	list := flags.String("sessions", "", "explicit comma-separated AO session ids")
	flags.Bool("json", false, "emit JSON (also the default)")
	var from, to string
	var interval, maxAge, maxGap time.Duration
	if command == "observe" {
		flags.DurationVar(&interval, "interval", 0, "wait after each completed sample")
	} else {
		flags.StringVar(&from, "from", "", "inclusive UTC RFC3339 timestamp")
		flags.StringVar(&to, "to", "", "exclusive UTC RFC3339 timestamp")
		flags.DurationVar(&maxAge, "max-age", 0, "explicit maximum fetchedAt age at sampling")
		flags.DurationVar(&maxGap, "max-gap", 0, "explicit maximum gap between sampling attempts")
	}
	if flags.Parse(args) != nil {
		return 2
	}
	ids := strings.Split(*list, ",")
	seen := map[string]bool{}
	valid := flags.NArg() == 0
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || seen[id] {
			valid = false
		}
		seen[id] = true
	}
	if !valid || (command == "observe" && interval <= 0) || (command == "reconcile" && (maxAge <= 0 || maxGap <= 0)) {
		fmt.Fprintln(stderr, "explicit unique sessions and positive durations required")
		return 2
	}
	var query usage.Cohort
	var err error
	if command == "reconcile" {
		query, err = usage.NewCohort(ids, from, to)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	// New durable data must never silently select the stable HOME namespace.
	if os.Getenv("AO_DATA_DIR") == "" || os.Getenv("AO_RUN_FILE") == "" {
		fmt.Fprintln(stderr, "explicit AO_DATA_DIR and AO_RUN_FILE required")
		return 1
	}
	client, err := source.FromEnvironment()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store := quota.Store{Root: client.DataRoot}
	if command == "observe" {
		err = quota.Observe(ctx, interval, func(ctx context.Context) error {
			tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			_, syncErr := scanQuotaSessions(tickCtx, client, ids)
			// Fixed existing CLI, same inherited explicit namespace, no refresh or
			// provider adapter. Neither stderr nor a raw execution error is persisted.
			raw, cliErr := exec.CommandContext(tickCtx, "ao", "account", "ls", "--json").Output()
			sample := quota.Project(raw, cliErr, client.DataRoot, time.Now())
			if err := store.Append(tickCtx, sample); err != nil {
				return err
			}
			syncStatus := "complete"
			if syncErr != nil {
				syncStatus = "unavailable"
			}
			return json.NewEncoder(stdout).Encode(struct {
				Sample     quota.Sample `json:"sample"`
				SyncStatus string       `json:"syncStatus"`
			}{sample, syncStatus})
		})
		if errors.Is(err, context.Canceled) {
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	rows, err := store.Read(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	scans, scanErr := scanQuotaSessions(ctx, client, ids)
	report, reportErr := quota.Reconcile(query, rows, scans, client.DataRoot, maxAge, maxGap)
	return writeUsageResult(report, errors.Join(scanErr, reportErr), stdout, stderr)
}
func scanQuotaSessions(ctx context.Context, client *source.Client, ids []string) ([]usage.Snapshot, error) {
	scans := make([]usage.Snapshot, 0, len(ids))
	var failures error
	for _, id := range ids {
		snapshot, err := client.Scan(ctx, id)
		scans = append(scans, snapshot)
		failures = errors.Join(failures, err)
	}
	return scans, failures
}
