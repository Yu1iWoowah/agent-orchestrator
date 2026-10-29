import type { components } from "../../../api/schema";

export function sessionQuestionFixture(
	overrides: Partial<components["schemas"]["QuestionResponse"]> = {},
): components["schemas"]["QuestionResponse"] {
	return {
		id: "ask-1",
		projectId: "project",
		originSessionId: "session",
		originHarness: "claude-code",
		audience: "user",
		kind: "question",
		clientKey: "deploy",
		state: "open",
		version: 1,
		sequence: 1,
		read: false,
		createdAt: "2026-10-29T00:00:00Z",
		updatedAt: "2026-10-29T00:00:00Z",
		payload: {
			schemaVersion: 1,
			title: "Choose a deployment target",
			explanation: "The build is ready. The target determines who can use it and the rollback impact.",
			questions: [
				{
					key: "target",
					header: "Target",
					question: "Where should this build be deployed?",
					explanation: "Choose the audience before deployment.",
					options: [
						{ id: "stage", label: "Staging", description: "Validate with internal users first.", recommended: true, preview: { format: "markdown", content: "```sh\ndeploy --target staging\n```" } },
						{ id: "prod", label: "Production", description: "Make it available to all users." },
					],
				},
				{
					key: "checks",
					header: "Checks",
					question: "Which checks should run first?",
					multiSelect: true,
					options: [
						{ id: "test", label: "Tests", description: "Run automated regression checks." },
						{ id: "review", label: "Review", description: "Request an independent review." },
					],
				},
			],
		},
		...overrides,
	};
}
