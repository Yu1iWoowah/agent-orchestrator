package cli

import (
	"github.com/spf13/cobra"
)

// Keep CLI wire shapes independent of controllers and the delegation runner.
type usageReportResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func newUsageCommand(c *commandContext) *cobra.Command {
	root := &cobra.Command{Use: "usage", Short: "Query usage through the daemon"}
	root.AddCommand(newUsageCompositionCommand(c))
	root.AddCommand(newDelegationCommand(c))
	return root
}
