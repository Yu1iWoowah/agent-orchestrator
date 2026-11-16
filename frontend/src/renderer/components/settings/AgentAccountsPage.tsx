import { AgentAccountsPanel } from "./AgentAccountsSection";

/** Retained account deep links use the same panels as the Harness Account tab. */
export function AgentAccountsPage() {
	return (
		<div className="min-w-0 space-y-5" data-testid="agent-accounts-page">
			<AgentAccountsPanel provider="claude" />
			<AgentAccountsPanel provider="codex" />
		</div>
	);
}
