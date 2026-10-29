import { useEffect, useId, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { LoaderCircle } from "lucide-react";
import { emptyQuestionFields, questionAnswers, type QuestionField, type QuestionFieldError, type SessionQuestion } from "../../lib/session-questions";
import { questionDraftKey, useQuestionDraftStore, type QuestionDraft } from "../../stores/question-draft-store";
import { sendQuestionAnswer } from "../../hooks/useQuestionAnswerRecovery";
import { labelForHost } from "../../lib/host-clients";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";
import { Badge } from "../ui/badge";
import { Textarea } from "../ui/textarea";
import { DecisionPreview } from "./DecisionPreview";
import type { MessageKey } from "../../i18n/messages";

const errorKeys = {
	required: "decision.error.required", single: "decision.error.single", unknown: "decision.error.unknown",
	otherRequired: "decision.error.otherRequired", otherTooLong: "decision.error.otherTooLong",
} satisfies Record<QuestionFieldError, MessageKey>;

function receiptFields(question: SessionQuestion) {
	const fields = emptyQuestionFields(question.payload);
	for (const answer of question.answer?.answers ?? []) {
		fields[answer.questionKey] = { selectedOptionIds: answer.selectedOptionIds, otherSelected: answer.otherText != null, otherText: answer.otherText ?? "" };
	}
	return fields;
}

export function QuestionDecisionView({ question, hostId, submitAllowed, onClose, onReload, compact = false }: {
	question: SessionQuestion; hostId?: string; submitAllowed: boolean; onClose: () => void; onReload?: () => void; compact?: boolean;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const entries = useQuestionDraftStore((state) => state.entries);
	const storageAvailable = useQuestionDraftStore((state) => state.storageAvailable);
	const key = questionDraftKey({ hostId, sessionId: question.originSessionId, questionId: question.id, version: question.version });
	const entry = entries[key];
	const [showErrors, setShowErrors] = useState(false);
	const prefix = useId();
	const groups = useRef(new Map<string, HTMLFieldSetElement>());
	const notice = useRef<HTMLDivElement>(null);
	const previousVersion = useRef(question.version);
	useEffect(() => {
		if (question.state === "open") useQuestionDraftStore.getState().ensure(question, hostId);
	}, [question.id, question.version, question.state, hostId]);
	useEffect(() => {
		if (entry && previousVersion.current !== question.version) {
			previousVersion.current = question.version;
			setShowErrors(false);
			notice.current?.focus();
		}
	}, [entry, question.version]);
	useEffect(() => {
		if (entry?.phase.state === "editing" && (entry.phase.errorCode || entry.phase.message)) notice.current?.focus();
	}, [entry?.phase]);
	const fields = question.state === "open" ? entry?.fields ?? emptyQuestionFields(question.payload) : receiptFields(question);
	const built = questionAnswers(question.payload, fields);
	const locked = question.state !== "open" || !entry || entry.phase.state !== "editing";
	const pending = entry?.phase.state === "submitting" || entry?.phase.state === "unknown";
	const references = Object.values(entries).filter((draft) =>
		(draft.scope.hostId ?? null) === (hostId ?? null) && draft.scope.sessionId === question.originSessionId && draft.scope.questionId === question.id &&
		(draft.scope.version !== question.version || question.state !== "open" || draft.phase.state === "conflict") &&
		Object.values(draft.fields).some((field) => field.selectedOptionIds.length || field.otherSelected || field.otherText));
	const changeField = (promptKey: string, field: QuestionField) => useQuestionDraftStore.getState().setField(key, promptKey, field);
	return <form className="flex min-h-0 flex-1 flex-col overflow-hidden" data-question-version={question.version}
		onKeyDown={(event) => { if (event.key === "Enter" && event.target instanceof HTMLInputElement) event.preventDefault(); }}
		onSubmit={(event) => {
			event.preventDefault();
			if (locked || !submitAllowed) return;
			setShowErrors(true);
			const first = question.payload.questions.find((prompt) => built.errors[prompt.key]);
			if (first) { const group = groups.current.get(first.key); group?.focus({ preventScroll: true }); group?.scrollIntoView?.({ block: "nearest" }); return; }
			if (useQuestionDraftStore.getState().begin(key, crypto.randomUUID(), Date.now())) void sendQuestionAnswer(client, key);
		}}>
		<div className="min-h-0 flex-1 space-y-5 overflow-y-auto break-words px-4 py-3">
			<h3 className="whitespace-pre-wrap text-sm font-medium text-foreground">{question.payload.title}</h3>
			<div className="space-y-1 text-xs text-muted-foreground">
				<p>{t("decision.source", { session: question.originSessionId, agent: question.originHarness })}</p>
				<p>{t("decision.host", { host: hostId ? labelForHost(hostId) ?? hostId : t("decision.localHost") })} · {t("decision.version", { version: question.version })}</p>
			</div>
			<p className="whitespace-pre-wrap text-sm text-foreground">{question.payload.explanation}</p>
			{question.payload.authorization ? <dl className="space-y-2 text-sm">
				{([ ["decision.scopeAction", question.payload.authorization.action], ["decision.scopeScope", question.payload.authorization.scope], ["decision.scopeLimits", question.payload.authorization.limits] ] as const).map(([label, value]) => <div key={label}><dt className="font-medium">{t(label)}</dt><dd className="whitespace-pre-wrap">{value}</dd></div>)}
				<div><dt className="font-medium">{t("decision.scopeEvidence")}</dt><dd><ul className="list-disc space-y-1 pl-5">{question.payload.authorization.evidence.map((text, index) => <li key={index} className="whitespace-pre-wrap">{text}</li>)}</ul></dd></div>
			</dl> : null}
			<div ref={notice} tabIndex={-1} className="space-y-3 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring">
				{references.map((draft) => <PreviousQuestionDraft key={questionDraftKey(draft.scope)} draft={draft} />)}
				{question.answer ? <p role="status" className="text-sm font-medium">{t(question.answer.deliveryState === "queued" ? "decision.savedQueued" : question.answer.deliveryState === "delivered" ? "decision.savedDelivered" : "decision.savedFailed")}</p> :
					question.state === "cancelled" || question.state === "expired" ? <p role="status" className="text-sm font-medium">{t(question.state === "cancelled" ? "decision.cancelled" : "decision.expired")}</p> : null}
				{question.answer?.lastError ? <details className="text-sm"><summary className="cursor-pointer">{t("decision.deliveryDetails")}</summary><p className="mt-2 whitespace-pre-wrap">{question.answer.lastError}</p></details> : null}
				{!submitAllowed && question.state === "open" ? <div className="space-y-2"><p role="alert" className="text-sm text-muted-foreground">{t("decision.sourceOffline")}</p>{compact && onReload ? <Button type="button" variant="outline" size="sm" onClick={onReload}>{t("decision.reload")}</Button> : null}</div> : null}
				{!storageAvailable ? <p role="alert" className="text-sm text-destructive">{t("decision.persistenceUnavailable")}</p> : null}
				{entry?.phase.state === "conflict" ? <p role="alert" className="text-sm text-destructive">{t("decision.conflict")}</p> : null}
				{entry?.phase.state === "unknown" ? <p role="status" className="text-sm">{t("decision.unknown")}</p> : null}
				{entry?.phase.state === "editing" && entry.phase.errorCode ? <p role="alert" className="text-sm text-destructive">{t("decision.answerTooLarge")}</p> : null}
				{entry?.phase.state === "editing" && entry.phase.message ? <div role="alert" className="space-y-1 text-sm text-destructive"><p>{t("decision.rejected")}</p>{Object.entries(entry.phase.fieldErrors ?? {}).map(([field, message]) => <p key={field}>{message}</p>)}</div> : null}
			</div>
			{question.state === "open" && !entry ? <p role="status">{t("decision.loading")}</p> : question.payload.questions.map((prompt) => {
				const field = fields[prompt.key];
				const error = showErrors ? built.errors[prompt.key] : undefined;
				const groupId = `${prefix}-${prompt.key}`;
				return <fieldset key={prompt.key} ref={(node) => { if (node) groups.current.set(prompt.key, node); else groups.current.delete(prompt.key); }} tabIndex={-1}
					className="min-w-0 space-y-2 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring" aria-invalid={Boolean(error)} aria-describedby={error ? `${groupId}-error` : undefined}>
					<legend className="mb-1 max-w-full space-y-1 text-sm"><span className="block text-xs text-muted-foreground">{prompt.header}</span><span className="block font-medium">{prompt.question}</span></legend>
					{prompt.explanation ? <p className="whitespace-pre-wrap text-sm text-muted-foreground">{prompt.explanation}</p> : null}
					<div className="space-y-3">
						{prompt.options.map((option) => {
							const id = `${groupId}-${option.id}`;
							return <div key={option.id} className="min-w-0">
								<div className="flex items-start gap-2.5">
									{prompt.multiSelect ? <Checkbox id={id} disabled={locked} checked={field.selectedOptionIds.includes(option.id)} aria-describedby={`${id}-description`}
										className="mt-0.5" onCheckedChange={(checked) => changeField(prompt.key, { ...field, selectedOptionIds: checked === true ? [...field.selectedOptionIds, option.id] : field.selectedOptionIds.filter((value) => value !== option.id) })} /> :
										<input id={id} type="radio" name={groupId} value={option.id} disabled={locked} checked={!field.otherSelected && field.selectedOptionIds.includes(option.id)} aria-describedby={`${id}-description`}
											className="mt-0.5 size-4 shrink-0 accent-primary outline-none focus-visible:ring-2 focus-visible:ring-ring" onChange={() => changeField(prompt.key, { ...field, selectedOptionIds: [option.id], otherSelected: false })} />}
									<div className="min-w-0 flex-1"><Label htmlFor={id} className="flex flex-wrap items-center gap-2 whitespace-pre-wrap break-words text-sm">{option.label}{option.recommended ? <Badge variant="neutral">{t("decision.recommended")}</Badge> : null}</Label>
										<p id={`${id}-description`} className="mt-1 whitespace-pre-wrap text-sm text-muted-foreground">{option.description}</p>
										{option.preview ? <DecisionPreview preview={option.preview} label={option.label} /> : null}
									</div>
								</div>
							</div>;
						})}
						<div className="flex items-start gap-2.5">
							{prompt.multiSelect ? <Checkbox id={`${groupId}-other`} disabled={locked} checked={field.otherSelected} className="mt-0.5" onCheckedChange={(checked) => changeField(prompt.key, { ...field, otherSelected: checked === true })} /> :
								<input id={`${groupId}-other`} type="radio" name={groupId} value="__other__" disabled={locked} checked={field.otherSelected} className="mt-0.5 size-4 shrink-0 accent-primary outline-none focus-visible:ring-2 focus-visible:ring-ring" onChange={() => changeField(prompt.key, { ...field, selectedOptionIds: [], otherSelected: true })} />}
							<div className="min-w-0 flex-1"><Label htmlFor={`${groupId}-other`}>{t("decision.other")}</Label>
								{field.otherSelected ? <div className="mt-2 space-y-1"><Label htmlFor={`${groupId}-other-text`} className="text-xs">{t("decision.otherAnswer", { header: prompt.header })}</Label>
									<Textarea id={`${groupId}-other-text`} value={field.otherText} readOnly={locked} aria-invalid={Boolean(error)} aria-describedby={`${groupId}-counter${error ? ` ${groupId}-error` : ""}`} onChange={(event) => changeField(prompt.key, { ...field, otherText: event.currentTarget.value })} />
									<p id={`${groupId}-counter`} className="text-right text-xs text-muted-foreground">{Array.from(field.otherText).length}/4000</p>
								</div> : null}
							</div>
						</div>
					</div>
					{error ? <p id={`${groupId}-error`} role="alert" className="text-sm text-destructive">{t(errorKeys[error])}</p> : null}
				</fieldset>;
			})}
		</div>
		<div className="shrink-0 space-y-2 border-t border-border px-4 py-3">
			{question.state === "open" && !compact ? <p className="text-xs text-muted-foreground">{t("decision.progress", { done: question.payload.questions.filter((prompt) => !built.errors[prompt.key]).length, total: question.payload.questions.length })}</p> : null}
			<div className="flex flex-wrap justify-end gap-2">
				{onReload && ((!submitAllowed && !compact) || entry?.phase.state === "conflict" || entry?.phase.state === "unknown") ? <Button type="button" variant="outline" onClick={onReload}>{t("decision.reload")}</Button> : null}
				<Button type="button" variant="ghost" onClick={onClose}>{t(question.state === "open" ? "decision.later" : "common.close")}</Button>
				{question.state === "open" && !(compact && (entry?.phase.state === "unknown" || entry?.phase.state === "conflict")) ? <Button type="submit" disabled={locked || !submitAllowed}>{pending ? <LoaderCircle aria-hidden="true" className="mr-2 size-4 animate-spin motion-reduce:animate-none" /> : null}{t(entry?.phase.state === "submitting" ? "decision.saving" : entry?.phase.state === "unknown" ? "decision.confirming" : "decision.submit")}</Button> : null}
			</div>
		</div>
	</form>;
}

function PreviousQuestionDraft({ draft }: { draft: QuestionDraft }) {
	const { t } = useTranslation();
	return <details className="text-sm"><summary className="cursor-pointer font-medium">{t("decision.previousDraft", { version: draft.scope.version })}</summary>
		<p className="mt-2 text-muted-foreground">{t("decision.previousDraftHint")}</p>
		<dl className="mt-2 space-y-3">{draft.question.payload.questions.map((prompt) => {
			const field = draft.fields[prompt.key];
			return <div key={prompt.key}><dt className="font-medium">{prompt.question}</dt><dd className="space-y-1 whitespace-pre-wrap">
				{field.selectedOptionIds.map((id) => <p key={id}>{prompt.options.find((option) => option.id === id)?.label ?? id}</p>)}
				{field.otherText ? <p>{t(field.otherSelected ? "decision.other" : "decision.unselectedOther")}: {field.otherText}</p> : null}
			</dd></div>;
		})}</dl>
	</details>;
}
