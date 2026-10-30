import { AgentAccountsPanel } from "./AgentAccountsSection";
/** Local account management shares the panels used by the existing Agents deep link. */
export function AgentAccountsPage() {
	return (
		<div className="min-w-0 space-y-5" data-testid="agent-accounts-page">
			<AgentAccountsPanel provider="claude" />
			<AgentAccountsPanel provider="codex" />
		</div>
	);
}
