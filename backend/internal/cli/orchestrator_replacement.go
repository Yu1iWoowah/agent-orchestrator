package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// These DTOs mirror the operation boundary without importing daemon services.
type orchestratorReplaceRequest struct {
	SourceSessionID string `json:"sourceSessionId"`
	OperationKey    string `json:"operationKey"`
}

type orchestratorPreparedRequest struct {
	Actor          projectDocumentActor `json:"actor"`
	AttemptID      string               `json:"attemptId"`
	Nonce          string               `json:"nonce"`
	Path           string               `json:"path"`
	Hash           string               `json:"hash"`
	WriteReceiptID string               `json:"writeReceiptId"`
}

type orchestratorReplacementResponse struct {
	Operation json.RawMessage `json:"operation"`
}

func newOrchestratorReplaceCommand(ctx *commandContext) *cobra.Command {
	var project, source, key string
	var asJSON bool
	cmd := &cobra.Command{
		Use: "replace", Short: "Accept a durable orchestrator handoff without waiting for retirement",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(project) == "" || strings.TrimSpace(source) == "" {
				return usageError{fmt.Errorf("--project and --from are required")}
			}
			if key == "" {
				key = uuid.NewString()
			}
			if err := replacementUUID(key); err != nil {
				return err
			}
			var res orchestratorReplacementResponse
			if err := ctx.postJSON(cmd.Context(), "projects/"+url.PathEscape(project)+"/orchestrator/replacements", orchestratorReplaceRequest{SourceSessionID: source, OperationKey: key}, &res); err != nil {
				return err
			}
			return writeReplacementResult(cmd, res, asJSON)
		},
	}
	cmd.Flags().StringVarP(&project, "project", "p", "", "Project id")
	cmd.Flags().StringVar(&source, "from", "", "Current orchestrator session id")
	cmd.Flags().StringVar(&key, "operation-key", "", "Canonical UUID for idempotent acceptance")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the durable operation as JSON")
	return cmd
}

func newOrchestratorReplacementCommand(ctx *commandContext) *cobra.Command {
	parent := &cobra.Command{Use: "replacement", Short: "Inspect or retry a durable orchestrator handoff"}
	for _, action := range []string{"get", "retry", "cancel"} {
		var asJSON bool
		cmd := &cobra.Command{
			Use: action + " <operation-id>", Short: map[string]string{"get": "Read the durable operation and its evidence", "retry": "Continue the same operation and successor", "cancel": "Cancel before source shutdown without interrupting agents"}[action],
			Args: exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := replacementUUID(args[0]); err != nil {
					return err
				}
				endpoint := "orchestrator/replacements/" + url.PathEscape(args[0])
				var res orchestratorReplacementResponse
				var err error
				if action == "get" {
					err = ctx.getJSON(cmd.Context(), endpoint, &res)
				} else {
					err = ctx.postJSON(cmd.Context(), endpoint+"/"+action, struct{}{}, &res)
				}
				if err != nil {
					return err
				}
				return writeReplacementResult(cmd, res, asJSON)
			},
		}
		cmd.Flags().BoolVar(&asJSON, "json", false, "Output the durable operation as JSON")
		parent.AddCommand(cmd)
	}
	parent.AddCommand(newOrchestratorPreparedCommand(ctx), newOrchestratorIntakeCommand(ctx), newReplacementDiagnoseCommand(ctx), newReplacementRepairCommand(ctx))
	return parent
}

func newOrchestratorIntakeCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "intake <operation-id>", Short: "Read canonical pending input and worker reports without acknowledging them", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := replacementUUID(args[0]); err != nil {
			return err
		}
		var response struct {
			Intake json.RawMessage `json:"intake"`
		}
		if err := ctx.getJSON(cmd.Context(), "orchestrator/replacements/"+url.PathEscape(args[0])+"/intake", &response); err != nil {
			return err
		}
		var summary struct {
			OperationID string            `json:"operationId"`
			Messages    []json.RawMessage `json:"messages"`
			Reports     []json.RawMessage `json:"reports"`
		}
		if err := json.Unmarshal(response.Intake, &summary); err != nil || summary.OperationID != args[0] {
			return fmt.Errorf("daemon returned no matching replacement intake identity")
		}
		if asJSON {
			return writeJSON(cmd.OutOrStdout(), response)
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Replacement %s: %d queued messages, %d pending reports; native delivery unconfirmed\nRead full payload: ao orchestrator replacement intake %s --json\n", summary.OperationID, len(summary.Messages), len(summary.Reports), summary.OperationID)
		return err
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output complete pending payloads and references as JSON")
	return cmd
}

func newOrchestratorPreparedCommand(ctx *commandContext) *cobra.Command {
	var in orchestratorPreparedRequest
	var asJSON bool
	cmd := &cobra.Command{Use: "prepared <operation-id>", Short: "Submit the owning source's managed handoff write receipt", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := replacementUUID(args[0]); err != nil {
			return err
		}
		for _, binding := range []struct{ name, value string }{{"--attempt", in.AttemptID}, {"--nonce", in.Nonce}} {
			if err := replacementUUID(binding.value); err != nil {
				return usageError{fmt.Errorf("%s: %w", binding.name, err)}
			}
		}
		if in.Path == "" || in.WriteReceiptID == "" {
			return usageError{fmt.Errorf("--path and --write-receipt-id are required")}
		}
		if _, err := hex.DecodeString(in.Hash); err != nil || len(in.Hash) != 64 {
			return usageError{fmt.Errorf("--hash must be a SHA-256 hex digest")}
		}
		in.Actor = projectDocumentActor{SessionID: strings.TrimSpace(os.Getenv("AO_SESSION_ID")), Capability: strings.TrimSpace(os.Getenv("AO_BROWSER_CAPABILITY"))}
		if in.Actor.SessionID == "" || in.Actor.Capability == "" {
			return usageError{fmt.Errorf("prepared requires AO_SESSION_ID and AO_BROWSER_CAPABILITY from the owning source")}
		}
		var res orchestratorReplacementResponse
		if err := ctx.postJSON(cmd.Context(), "orchestrator/replacements/"+url.PathEscape(args[0])+"/prepared", in, &res); err != nil {
			return err
		}
		return writeReplacementResult(cmd, res, asJSON)
	}}
	cmd.Flags().StringVar(&in.AttemptID, "attempt", "", "Preparation attempt UUID from the daemon prompt")
	cmd.Flags().StringVar(&in.Nonce, "nonce", "", "Preparation nonce UUID from the daemon prompt")
	cmd.Flags().StringVar(&in.Path, "path", "", "Configured handoff relative path")
	cmd.Flags().StringVar(&in.Hash, "hash", "", "Hash returned by the managed document write")
	cmd.Flags().StringVar(&in.WriteReceiptID, "write-receipt-id", "", "Receipt ID returned by the managed document write")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the durable operation as JSON")
	return cmd
}

func replacementUUID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return usageError{fmt.Errorf("replacement id and operation key must be canonical nonzero UUIDs")}
	}
	return nil
}

func writeReplacementResult(cmd *cobra.Command, res orchestratorReplacementResponse, asJSON bool) error {
	var summary struct {
		ID           string `json:"id"`
		Phase        string `json:"phase"`
		RetryCommand string `json:"retryCommand"`
	}
	if err := json.Unmarshal(res.Operation, &summary); err != nil || summary.ID == "" || summary.Phase == "" {
		return fmt.Errorf("daemon returned no replacement operation identity or phase")
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Replacement %s: %s\n", summary.ID, summary.Phase); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Read: ao orchestrator replacement get %s --json\n", summary.ID); err != nil {
		return err
	}
	if summary.RetryCommand != "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Retry: %s\n", summary.RetryCommand)
		return err
	}
	return nil
}
