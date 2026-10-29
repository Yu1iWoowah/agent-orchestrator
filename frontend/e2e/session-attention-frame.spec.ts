import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

test("attention fixture preserves counts through history, collapse and composer growth @T0", async ({
	page,
}) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await page.goto(
		"/e2e/session-attention/harness.html?session=attention-browser-fixture",
	);
	const frame = page.getByRole("region", { name: "Awaiting you", exact: true });
	await expect(frame.getByText("3", { exact: true })).toBeVisible();
	await frame.getByRole("tab", { name: "History", exact: true }).click();
	await expect(frame.getByText("Previously answered fixture")).toBeVisible();
	await expect(
		frame.getByRole("button", { name: /^Open decision:/ }),
	).toHaveCount(1);
	await frame
		.getByRole("button", { name: "Collapse Awaiting you", exact: true })
		.click();
	await page
		.getByRole("button", { name: "Awaiting you 3", expanded: false })
		.press("Enter");
	await expect(frame.getByText("Previously answered fixture")).toBeVisible();
	await page
		.getByRole("button", { name: "Composer queue", exact: true })
		.click();
	await expect
		.poll(async () => {
			const a = await frame.boundingBox(),
				b = await page.locator(".cursor-chat-composer-dock").boundingBox();
			return a && b ? b.y - a.y - a.height : -1;
		})
		.toBeGreaterThanOrEqual(15);
});

for (const resolution of ["local", "external"] as const) {
	test(`attention releases ${resolution} resolved sources for later approvals and inputs @T0`, async ({
		page,
	}) => {
		const sessionId = "attention-frame";
		const now = "2026-10-29T00:00:00Z";
		let answered = false;
		let newApproval = false;
		let approved = false;
		const seen = new Set<string>();
		const answers: unknown[] = [];
		await page.setViewportSize({ width: 1440, height: 1000 });
		await installFakeAgent(page, {
			workers: [
				{
					id: sessionId,
					title: "Attention with sources beyond the loaded transcript",
					mode: "chat",
				},
			],
		});
		const item = (id: string, title: string, sequence: number) => ({
			branchId: "root",
			actionable: !(answered && id === "older-input"),
			read: seen.has(id),
			activity: {
				kind: "activity",
				id,
				requestId: `native-${id}`,
				activityKind: "user_input",
				status: answered && id === "older-input" ? "resolved" : "pending",
				sequence,
				revision: 0,
				summary: title,
				createdAt: now,
				detail: {
					inputMode: "form",
					message: title,
					schema: {
						type: "object",
						required: ["target"],
						properties: { target: { type: "string", title: "Target" } },
					},
				},
			},
		});
		const approvalItem = () => ({
			...item(
				"followup-approval",
				"New approval after the located question",
				252,
			),
			actionable: !approved,
			activity: {
				...item(
					"followup-approval",
					"New approval after the located question",
					252,
				).activity,
				turnId: "live",
				activityKind: "approval",
				status: approved ? "resolved" : "pending",
				detail: {
					command: "echo fixture",
					decisions: [{ id: "allow", label: "Allow once", kind: "allow_once" }],
				},
			},
		});
		const followupInput = () => ({
			...item("followup-input", "New input after the located approval", 253),
			activity: {
				...item("followup-input", "New input after the located approval", 253)
					.activity,
				turnId: "live",
			},
		});
		const items = () => [
			...(newApproval ? [approvalItem()] : []),
			...(approved ? [followupInput()] : []),
			item("older-input", "Question outside the loaded transcript", 1),
			item("next-input", "A second original request", 2),
			...Array.from({ length: 7 }, (_, i) =>
				item(`other-${i}`, `Another pending request ${i + 1}`, i + 3),
			),
		];
		await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
			const url = new URL(route.request().url()),
				path = url.pathname;
			if (path.endsWith("/questions")) {
				await route.fulfill({ json: { items: [], open: 0, unread: 0, history: 0, hasMoreBefore: false, nextBeforeSequence: 0 } });
			} else if (path.endsWith("/attention")) {
				const all = items();
				const history = url.searchParams.get("state") === "history";
				await route.fulfill({
					json: {
						conversationId: "attention-conversation",
						activeBranchId: "root",
						counts: {
							open: all.filter((i) => i.actionable).length,
							unread: all.filter((i) => i.actionable && !i.read).length,
							history: answered ? 1 : 0,
						},
						items: all.filter((i) => (history ? !i.actionable : i.actionable)),
						hasMoreBefore: false,
					},
				});
			} else if (path.includes("/attention/") && path.endsWith("/read")) {
				const id = path.split("/").at(-2)!;
				seen.add(id);
				expect(route.request().postDataJSON()).toEqual({
					expectedBranchId: "root",
					expectedRevision: 0,
				});
				await route.fulfill({ status: 204 });
			} else if (path.includes("/attention/")) {
				const source = items().find(
					(i) => i.activity.id === path.split("/").at(-1),
				);
				await route.fulfill({
					json: {
						conversationId: "attention-conversation",
						activeBranchId: "root",
						counts: {
							open: answered ? 8 : 9,
							unread: 0,
							history: answered ? 1 : 0,
						},
						items: source ? [source] : [],
						hasMoreBefore: false,
					},
				});
			} else if (path.endsWith("/approvals/native-followup-approval/resolve")) {
				expect(route.request().postDataJSON()).toEqual({ decisionId: "allow" });
				approved = true;
				await route.fulfill({ status: 204 });
			} else if (path.endsWith("/inputs/native-older-input/resolve")) {
				answers.push(route.request().postDataJSON());
				answered = true;
				await route.fulfill({ status: 204 });
			} else if (path.endsWith("/plan"))
				await route.fulfill({
					json: {
						state: "absent",
						conversationId: "attention-conversation",
						activeBranchId: "root",
					},
				});
			else if (path.endsWith("/conversation"))
				await route.fulfill({
					json: {
						conversationId: "attention-conversation",
						activeBranchId: "root",
						sessionId,
						harness: "claude-code",
						mode: "chat",
						controller: "ready",
						settings: {},
						oldestSequence: 250,
						latestSequence: approved ? 253 : newApproval ? 252 : 251,
						hasMoreBefore: false,
						turns: [{ id: "live", state: "running", requestedAt: now }],
						messages: [
							{
								kind: "message",
								id: "recent",
								turnId: "live",
								sequence: 250,
								revision: 0,
								role: "user",
								origin: "human",
								text: "Newer transcript without the pending source",
								streaming: false,
								createdAt: now,
							},
						],
						activities: items()
							.filter((i) =>
								["followup-approval", "followup-input"].includes(i.activity.id),
							)
							.map((i) => i.activity),
					},
				});
			else if (path.endsWith("/conversation/models"))
				await route.fulfill({ json: { models: [], selected: {} } });
			else if (path.endsWith("/conversation/skills"))
				await route.fulfill({ json: { skills: [] } });
			else if (path.endsWith("/workspace/files"))
				await route.fulfill({ json: { files: [], truncated: false } });
			else
				await route.fulfill({
					status: 404,
					json: { code: "NOT_FOUND", message: "not found" },
				});
		});
		await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
		const frame = page.getByRole("region", {
			name: "Awaiting you",
			exact: true,
		});
		await expect(frame.getByText("9", { exact: true })).toBeVisible();
		await expect(frame.getByRole("textbox")).toHaveCount(0);
		await frame
			.getByRole("button", {
				name: "Open decision: Question outside the loaded transcript",
				exact: true,
			})
			.click();
		await page.getByRole("dialog").getByRole("button", { name: "Go to original request", exact: true }).click();
		const form = page.getByTestId("elicitation-dock");
		await expect(form).toHaveCount(1);
		const input = form.getByRole("textbox", { name: "Target" });
		await expect(input).toBeFocused();
		await input.fill("staging");
		await expect.poll(() => seen.has("older-input")).toBe(true);
		expect(answers).toHaveLength(0);
		await frame
			.getByRole("button", {
				name: "Open decision: A second original request",
				exact: true,
			})
			.click();
		await page.getByRole("dialog").getByRole("button", { name: "Go to original request", exact: true }).click();
		await expect(form.getByRole("textbox", { name: "Target" })).toHaveValue("");
		await frame
			.getByRole("button", {
				name: "Open decision: Question outside the loaded transcript",
				exact: true,
			})
			.click();
		await page.getByRole("dialog").getByRole("button", { name: "Go to original request", exact: true }).click();
		await expect(form.getByRole("textbox", { name: "Target" })).toHaveValue(
			"staging",
		);
		await expect(form).toHaveCount(1);
		const frameBox = await frame.boundingBox(),
			composer = await page.locator(".cursor-chat-composer-dock").boundingBox();
		if (!frameBox || !composer) throw Error("Missing geometry");
		expect(frameBox.y + frameBox.height).toBeLessThanOrEqual(composer.y - 15);
		if (resolution === "local") {
			await form.getByRole("button", { name: "Continue", exact: true }).click();
			await expect.poll(() => answers.length).toBe(1);
			expect(answers[0]).toEqual({
				action: "accept",
				content: { target: "staging" },
			});
		} else {
			// Authoritative provider/other-client completion, without this form submitting.
			answered = true;
		}
		await page.evaluate(
			(id) => window.__aoFakeAgent!.conversationChanged(id, "attention-conversation"),
			sessionId,
		);
		await expect(frame.getByText("8", { exact: true })).toBeVisible();
		await expect(form).toHaveCount(0);
		await frame.getByRole("tab", { name: "History" }).click();
		await expect(
			frame.getByText("Question outside the loaded transcript"),
		).toBeVisible();
		await expect(frame.getByText("Answered", { exact: true })).toBeVisible();
		expect(answers).toHaveLength(resolution === "local" ? 1 : 0);
		newApproval = true;
		await page.evaluate(
			(id) => window.__aoFakeAgent!.conversationChanged(id, "attention-conversation"),
			sessionId,
		);
		const allow = page
			.locator(".cursor-chat-composer-dock")
			.getByRole("button", { name: /^Allow once/ });
		await expect(allow).toBeVisible();
		await frame.getByRole("tab", { name: "Pending", exact: true }).click();
		await frame
			.getByRole("button", {
				name: "Open decision: New approval after the located question",
				exact: true,
			})
			.click();
		await page.getByRole("dialog").getByRole("button", { name: "Go to original request", exact: true }).click();
		await allow.click();
		await expect.poll(() => approved).toBe(true);
		await page.evaluate(
			(id) => window.__aoFakeAgent!.conversationChanged(id, "attention-conversation"),
			sessionId,
		);
		await expect(
			form.getByText("New input after the located approval"),
		).toBeVisible();
		await expect(form.getByRole("textbox", { name: "Target" })).toHaveValue("");
	});
}
