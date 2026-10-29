import { describe, expect, it } from "vitest";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { emptyQuestionFields, parseSessionQuestion, parseSessionQuestionPage, questionAnswers } from "./session-questions";

describe("structured question boundary", () => {
	it("keeps all grouped questions and starts with zero selections", () => {
		const question = parseSessionQuestion(sessionQuestionFixture(), "session");
		const fields = emptyQuestionFields(question.payload);
		expect(fields.target.selectedOptionIds).toEqual([]);
		expect(fields.checks.selectedOptionIds).toEqual([]);
		expect(fields.target.otherSelected).toBe(false);
		expect(questionAnswers(question.payload, fields).errors).toEqual({ target: "required", checks: "required" });
	});

	it.each([
		{ ...sessionQuestionFixture(), originSessionId: "foreign" },
		{ ...sessionQuestionFixture(), version: Number.MAX_SAFE_INTEGER + 1 },
		{ ...sessionQuestionFixture(), state: "unknown" },
		{ ...sessionQuestionFixture(), payload: { ...sessionQuestionFixture().payload, schemaVersion: 2 } },
		{ ...sessionQuestionFixture(), payload: { ...sessionQuestionFixture().payload, questions: [] } },
		{ ...sessionQuestionFixture(), payload: { ...sessionQuestionFixture().payload, explanation: " " } },
	])("rejects invalid or out-of-scope response", (value) => {
		expect(() => parseSessionQuestion(value, "session")).toThrow();
	});

	it("rejects malformed choices, recommendations and unsafe previews without dropping them", () => {
		for (const patch of [
			{ options: [{ id: "only", label: "Only", description: "One is insufficient" }] },
			{ options: [
				{ id: "first", label: "First", description: "A", recommended: true },
				{ id: "second", label: "Second", description: "B", recommended: true },
			] },
			{ options: [
				{ id: "first", label: "First", description: "A" },
				{ id: "second", label: "Second", description: "B", recommended: true },
			] },
			{ options: [
				{ id: "first", label: "Other", description: "A" },
				{ id: "second", label: "Second", description: "B" },
			] },
			{ options: [
				{ id: "first", label: "First", description: "A", preview: { format: "markdown", content: "![image](https://example.com/image.png)" } },
				{ id: "second", label: "Second", description: "B" },
			] },
		]) {
			const question = sessionQuestionFixture();
			const value = { ...question, payload: { ...question.payload, questions: [{ ...question.payload.questions[0], ...patch }] } };
			expect(() => parseSessionQuestion(value, "session")).toThrow();
		}
	});

	it("preserves complete page counts and rejects partial invalid pages", () => {
		const page = { items: [sessionQuestionFixture()], open: 7, unread: 3, history: 2, hasMoreBefore: true, nextBeforeSequence: 1 };
		expect(parseSessionQuestionPage(page, "session")).toEqual(page);
		expect(() => parseSessionQuestionPage({ ...page, unread: -1 }, "session")).toThrow();
		expect(() => parseSessionQuestionPage({ ...page, items: [sessionQuestionFixture(), { id: "broken" }] }, "session")).toThrow();
	});
});

describe("grouped answer construction", () => {
	it("allows single Other and multi-select plus Other, in payload order", () => {
		const { payload } = sessionQuestionFixture();
		const fields = emptyQuestionFields(payload);
		fields.target = { selectedOptionIds: [], otherSelected: true, otherText: "Use the private preview" };
		fields.checks = { selectedOptionIds: ["review", "test"], otherSelected: true, otherText: "Check rollback too" };
		expect(questionAnswers(payload, fields)).toEqual({
			errors: {},
			answers: [
				{ questionKey: "target", selectedOptionIds: [], otherText: "Use the private preview" },
				{ questionKey: "checks", selectedOptionIds: ["test", "review"], otherText: "Check rollback too" },
			],
		});
	});

	it("keeps unselected Other text locally and never includes it in the answer", () => {
		const { payload } = sessionQuestionFixture(); const fields = emptyQuestionFields(payload);
		fields.target = { selectedOptionIds: ["stage"], otherSelected: false, otherText: "unsent draft" };
		fields.checks.selectedOptionIds = ["test"];
		const result = questionAnswers(payload, fields);
		expect(result.errors).toEqual({});
		expect(result.answers[0]).not.toHaveProperty("otherText");
		expect(fields.target.otherText).toBe("unsent draft");
	});

	it("rejects single-choice combinations, unknown or duplicate IDs and missing groups", () => {
		const { payload } = sessionQuestionFixture();
		for (const target of [
			{ selectedOptionIds: ["stage", "prod"], otherSelected: false, otherText: "" },
			{ selectedOptionIds: ["stage"], otherSelected: true, otherText: "extra" },
			{ selectedOptionIds: ["missing"], otherSelected: false, otherText: "" },
			{ selectedOptionIds: ["stage", "stage"], otherSelected: false, otherText: "" },
		]) {
			const result = questionAnswers(payload, { target });
			expect(result.errors.target).toBeDefined();
			expect(result.errors.checks).toBe("required");
			expect(result.answers).toEqual([]);
		}
	});

	it("validates Other using Unicode code points", () => {
		const { payload } = sessionQuestionFixture(); const fields = emptyQuestionFields(payload);
		fields.target = { selectedOptionIds: [], otherSelected: true, otherText: "🚀".repeat(4000) };
		fields.checks.selectedOptionIds = ["test"];
		expect(questionAnswers(payload, fields).errors).toEqual({});
		fields.target.otherText += "🚀";
		expect(questionAnswers(payload, fields).errors.target).toBe("otherTooLong");
		fields.target.otherText = " \n";
		expect(questionAnswers(payload, fields).errors.target).toBe("otherRequired");
	});
});
