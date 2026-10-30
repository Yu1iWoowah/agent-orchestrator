package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

type replacementRepairRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	ExpectedDigest   string `json:"expectedDigest"`
	SnapshotRevision int64  `json:"snapshotRevision"`
	SnapshotDigest   string `json:"snapshotDigest"`
}

func newReplacementDiagnoseCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{Use: "diagnose <operation-id>", Short: "Read operator row and sealed snapshot identities without raw state", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := replacementUUID(args[0]); err != nil {
			return err
		}
		var response struct {
			Diagnostic json.RawMessage `json:"diagnostic"`
		}
		if err := ctx.getJSON(cmd.Context(), "orchestrator/replacements/"+url.PathEscape(args[0])+"/diagnose", &response); err != nil {
			return err
		}
		var identity struct {
			OperationID string `json:"operationId"`
		}
		if err := json.Unmarshal(response.Diagnostic, &identity); err != nil || identity.OperationID != args[0] {
			return fmt.Errorf("daemon returned no matching diagnostic identity")
		}
		// CAS metadata is useful as exact JSON in both human and scripted use.
		return writeJSON(cmd.OutOrStdout(), response)
	}}
	cmd.Flags().Bool("json", false, "Output operator CAS metadata as JSON")
	return cmd
}

func newReplacementRepairCommand(ctx *commandContext) *cobra.Command {
	var in replacementRepairRequest
	var asJSON bool
	cmd := &cobra.Command{Use: "repair <operation-id>", Short: "Restore a corrupt row from the explicitly authorized sealed snapshot", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := replacementUUID(args[0]); err != nil {
			return err
		}
		if in.ExpectedRevision < 0 || in.SnapshotRevision < 0 {
			return usageError{fmt.Errorf("--expected-revision and --snapshot-revision are required")}
		}
		for _, digest := range []string{in.ExpectedDigest, in.SnapshotDigest} {
			if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
				return usageError{fmt.Errorf("--expected-digest and --snapshot-digest must be SHA-256 hex digests")}
			}
		}
		var response orchestratorReplacementResponse
		if err := ctx.postJSON(cmd.Context(), "orchestrator/replacements/"+url.PathEscape(args[0])+"/repair", in, &response); err != nil {
			return err
		}
		return writeReplacementResult(cmd, response, asJSON)
	}}
	cmd.Flags().Int64Var(&in.ExpectedRevision, "expected-revision", -1, "Raw row revision from diagnose")
	cmd.Flags().StringVar(&in.ExpectedDigest, "expected-digest", "", "Complete raw row SHA-256 from diagnose")
	cmd.Flags().Int64Var(&in.SnapshotRevision, "snapshot-revision", -1, "Explicitly authorized sealed snapshot revision")
	cmd.Flags().StringVar(&in.SnapshotDigest, "snapshot-digest", "", "Explicitly authorized sealed snapshot SHA-256")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the data-only repair receipt as JSON")
	return cmd
}
