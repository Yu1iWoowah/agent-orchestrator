import { connect } from "node:net";
import { randomUUID } from "node:crypto";
import { StringDecoder } from "node:string_decoder";
import type { MessageBoxOptions, MessageBoxReturnValue } from "electron";

const purposes = new Map([
 ["handoff.required_context", { en: "Confirm required handoff context", zh: "確認交接必要內容", riskEn: "This confirms the fixed context digest. It does not authorize handoff recovery or permission restoration.", riskZh: "僅確認固定內容 digest，不授權交接恢復或權限恢復。" }],
 ["handoff.retry_source_restore", { en: "Allow source restoration attempt", zh: "允許嘗試恢復來源", riskEn: "The handoff service must still verify the recovery epoch, owner and effect gate before restoring the source.", riskZh: "交接服務仍須重驗恢復 epoch、owner 與 effect gate，才可嘗試恢復來源。" }],
 ["handoff.recover_promote_verified_candidate", { en: "Allow promotion of the verified candidate", zh: "允許提升已驗證候選", riskEn: "This can change the handoff writer. Candidate verification and current authority remain the handoff service's responsibility.", riskZh: "後續可能改變交接 writer；候選驗證與目前 authority 仍由交接服務負責。" }],
 ["handoff.abandon_source_stopped", { en: "Allow abandoning the stopped source", zh: "允許放棄已停止的來源", riskEn: "This is the stopped-source exit, not permission to restart a source or restore session settings.", riskZh: "這是保留來源停止狀態的出口，不授權重新啟動來源或恢復 session 設定。" }],
 ["settings.rebind", { en: "Allow rebinding session settings", zh: "允許重新綁定 session 設定", riskEn: "Account, native and settings bindings may change. The settings service must recheck the fixed scope and revisions.", riskZh: "後續可能改變帳號、native 與設定綁定；設定服務須重驗固定 scope 與 revisions。" }],
 ["settings.rebind_idle_batch", { en: "Allow rebinding the fixed idle batch", zh: "允許重新綁定固定閒置清單", riskEn: "Only the listed batch is covered. Each item must pass its own eligibility checks before execution.", riskZh: "只涵蓋列出的固定清單；各項執行前仍須重驗資格，不得加入新 session。" }],
 ["settings.restore_permissions", { en: "Allow restoring session permissions", zh: "允許恢復 session 權限", riskEn: "Session permissions may change. Handoff recovery consent cannot replace this separate permission decision.", riskZh: "後續可能改變 session 權限；交接恢復同意不能替代這次獨立的權限決定。" }],
 ["delivery.unknown.defer", { en: "Allow deferring the unknown delivery", zh: "允許稍後處理 unknown delivery", riskEn: "The delivery remains uncertain. This must not mark it sent or delivered.", riskZh: "delivery 的不確定事實必須保留，不得標成 sent 或 delivered。" }],
 ["delivery.linked_resend", { en: "Allow the linked resend", zh: "允許有 lineage 的重新傳送", riskEn: "Resending can duplicate delivery. The original lineage and eligibility checks must be preserved.", riskZh: "重新傳送可能造成重複 delivery；必須保留原 lineage 並重驗資格。" }],
]);

type Plan = Readonly<{
	protocol: "operator-consent/1";
	purpose: string;
	domain: string;
	actionKey: string;
	subjectRef: string;
	participantRefs: readonly string[];
	scope: Readonly<{ projectRef: string; ownerRef: string; branchRef: string; nativeRef: string }>;
	expectedRevisions: Readonly<Record<string, number>>;
	payloadDigest: string;
	effectDigest: string;
	validUntil: string;
	riskCodes: readonly string[];
}>;
type Review = Readonly<{ requestRef: string; challenge: string; digest: string; plan: Plan; explanation: { en: string; zh: string; riskEn: string; riskZh: string } }>;
type Reply = { kind: "rejected"; retryable: boolean } | { kind: "hello" | "pong" | "confirmed" | "cancelled"; requestId?: string } | { kind: "review"; review: Review; requestId: string } | { kind: "error"; requestId?: string };
type NativeDialog = (options: MessageBoxOptions) => Promise<MessageBoxReturnValue>;

function object(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}
function reference(value: unknown): string {
	if (typeof value !== "string" || !/^[a-zA-Z0-9._:/@+-]{1,160}$/.test(value)) throw new Error("OPERATOR_PROTOCOL_INVALID");
	return value;
}
function digest(value: unknown): string {
	if (typeof value !== "string" || !/^[a-f0-9]{64}$/.test(value)) throw new Error("OPERATOR_PROTOCOL_INVALID");
	return value;
}
function identifiers(value: unknown, limit: number): readonly string[] {
	if (!Array.isArray(value) || value.length < 1 || value.length > limit) throw new Error("OPERATOR_PROTOCOL_INVALID");
	return Object.freeze(value.map(reference));
}

function parseReview(value: Record<string, unknown>): Review {
	const p = value.plan;
	if (!object(p) || p.protocol !== "operator-consent/1" || typeof p.purpose !== "string" || !purposes.has(p.purpose) ||
		typeof p.validUntil !== "string" || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,3})?Z$/.test(p.validUntil) || !Number.isFinite(Date.parse(p.validUntil)) || !object(p.scope) || !object(p.expectedRevisions)) throw new Error("OPERATOR_PROTOCOL_INVALID");
	const explanation = purposes.get(p.purpose);
	if (!explanation) throw new Error("OPERATOR_PROTOCOL_INVALID");
	const fields = ["protocol", "purpose", "domain", "actionKey", "subjectRef", "participantRefs", "scope", "expectedRevisions", "payloadDigest", "effectDigest", "validUntil", "riskCodes"];
	if (Object.keys(p).length !== fields.length || Object.keys(p).some((key) => !fields.includes(key)) || Object.keys(p.scope).length !== 4) throw new Error("OPERATOR_PROTOCOL_INVALID");
	const revisions: Record<string, number> = {};
	const entries = Object.entries(p.expectedRevisions);
	if (entries.length < 1 || entries.length > 64) throw new Error("OPERATOR_PROTOCOL_INVALID");
	for (const [key, revision] of entries) {
		if (typeof revision !== "number" || !Number.isSafeInteger(revision) || revision < 0) throw new Error("OPERATOR_PROTOCOL_INVALID");
		Object.defineProperty(revisions, reference(key), { value: revision, enumerable: true });
	}
	const risks = identifiers(p.riskCodes, 16);
	if (risks.some((risk) => !/^[A-Z_]{1,64}$/.test(risk))) throw new Error("OPERATOR_PROTOCOL_INVALID");
	return Object.freeze({ requestRef: reference(value.requestRef), challenge: reference(value.challenge), digest: digest(value.digest), explanation, plan: Object.freeze({
		protocol: p.protocol, purpose: p.purpose, domain: reference(p.domain), actionKey: reference(p.actionKey), subjectRef: reference(p.subjectRef),
		participantRefs: identifiers(p.participantRefs, 64), scope: Object.freeze({ projectRef: reference(p.scope.projectRef), ownerRef: reference(p.scope.ownerRef), branchRef: reference(p.scope.branchRef), nativeRef: reference(p.scope.nativeRef) }),
		expectedRevisions: Object.freeze(revisions), payloadDigest: digest(p.payloadDigest), effectDigest: digest(p.effectDigest), validUntil: p.validUntil, riskCodes: risks,
	}) });
}

function parseReply(text: string): Reply {
	const value: unknown = JSON.parse(text);
	if (!object(value)) throw new Error("OPERATOR_PROTOCOL_INVALID");
	const requestId = value.requestId === undefined ? undefined : reference(value.requestId);
	switch (value.type) {
		case "operator-hello-ok":
			if (value.version !== 1) throw new Error("OPERATOR_PROTOCOL_INVALID");
			return { kind: "hello" };
		case "operator-hello-rejected": return { kind: "rejected", retryable: value.code === "OPERATOR_PIN_PENDING" || value.code === "OPERATOR_ISSUER_BUSY" };
		case "pong": return { kind: "pong" };
		case "review-plan": return { kind: "review", review: parseReview(value), requestId: reference(requestId) };
		case "confirmed": return { kind: "confirmed", requestId };
		case "cancelled": return { kind: "cancelled", requestId };
		case "error": return { kind: "error", requestId };
		default: throw new Error("OPERATOR_PROTOCOL_INVALID");
	}
}

export function operatorConsentLabels(zh: boolean) {
	return zh ? { menu: "操作者確認", review: "檢視下一個固定操作計畫…", notReady: "本機確認 issuer 尚未就緒。" } :
		{ menu: "Operator confirmation", review: "Review next fixed action plan…", notReady: "The native confirmation issuer is not ready." };
}

// Only Electron main calls this. No credential, plan or confirm method crosses
// preload, renderer, Browser automation, ordinary HTTP, environment or files.
export function connectOperatorConsent(options: { address: string; ownedToken?: string; show: NativeDialog; zh: () => boolean }) {
	let disposed = false;
	let connected = false;
	let busy = false;
	let retryCount = 0;
	let stopRetry = false;
	let retry: ReturnType<typeof setTimeout> | undefined;
	let heartbeat: ReturnType<typeof setInterval> | undefined;
	let socket: ReturnType<typeof connect> | undefined;
	let lifetime = new AbortController();
	let pending: { id: string; resolve: (reply: Reply) => void; reject: () => void; timer: ReturnType<typeof setTimeout> } | undefined;
	const send = (frame: object) => socket?.write(`${JSON.stringify(frame)}\n`);
	const call = (frame: object): Promise<Reply> => new Promise((resolve, reject) => {
		if (!connected || pending) { reject(new Error("OPERATOR_ISSUER_UNAVAILABLE")); return; }
		const fail = () => { pending = undefined; reject(new Error("OPERATOR_ISSUER_UNAVAILABLE")); };
		const id = randomUUID();
		pending = { id, resolve, reject: fail, timer: setTimeout(fail, 5_000) };
		send({ ...frame, requestId: id });
	});
	const start = () => {
		if (disposed) return;
		const current = connect({ path: options.address });
		socket = current;
		const handshake = setTimeout(() => current.destroy(), 5_000);
		const decoder = new StringDecoder("utf8");
		let buffer = "";
		current.once("connect", () => send({ type: "operator-hello", version: 1, token: options.ownedToken }));
		current.on("error", () => current.destroy());
		current.on("data", (data: Buffer) => {
			if (disposed || socket !== current) return;
			try {
				buffer += decoder.write(data);
				if (Buffer.byteLength(buffer) > 65536) throw new Error("OPERATOR_PROTOCOL_INVALID");
				for (;;) {
					const end = buffer.indexOf("\n");
					if (end < 0) break;
					const reply = parseReply(buffer.slice(0, end));
					buffer = buffer.slice(end + 1);
					if (reply.kind === "hello") {
						if (connected) throw new Error("OPERATOR_PROTOCOL_INVALID");
						connected = true;
						retryCount = 0;
						clearTimeout(handshake);
						lifetime = new AbortController();
						send({ type: "ready" });
						heartbeat = setInterval(() => send({ type: "ping" }), 5_000);
					} else if (reply.kind === "rejected") {
						if (!reply.retryable) stopRetry = true;
						current.destroy();
					} else if (reply.kind !== "pong" && pending && reply.requestId === pending.id) {
						const receiver = pending;
						clearTimeout(receiver.timer);
						pending = undefined;
						receiver.resolve(reply);
					}
				}
			} catch { stopRetry = true; current.destroy(); }
		});
		current.once("close", () => {
			connected = false;
			clearTimeout(handshake);
			lifetime.abort();
			clearInterval(heartbeat);
			if (pending) { clearTimeout(pending.timer); pending.reject(); }
			if (!disposed && !stopRetry) {
				retryCount++;
				if (retryCount < 6) retry = setTimeout(start, Math.min(2_000 * 2 ** (retryCount - 1), 30_000));
			}
		});
	};
	start();
	return {
		dispose() {
			disposed = true;
			connected = false;
			lifetime.abort();
			clearInterval(heartbeat);
			clearTimeout(retry);
			socket?.destroy();
		},
		async reviewNext() {
			if (busy) return;
			busy = true;
			const zh = options.zh();
			try {
				const response = await call({ type: "review-next" });
				if (response.kind !== "review") throw new Error("OPERATOR_ACTION_UNAVAILABLE");
				const review = response.review;
				const label = zh ? review.explanation.zh : review.explanation.en;
				const signal = lifetime.signal;
				// Paginate without truncating any fixed field, including batch members.
				const text = JSON.stringify(review.plan, null, 2);
				const pages: string[] = [];
				let page = "";
				let lines = 0;
				for (const line of text.match(/[^\n]*\n|[^\n]+$/g) ?? []) {
					if (page && (page.length + line.length > 1000 || lines >= 12)) {
						pages.push(page);
						page = "";
						lines = 0;
					}
					page += line;
					lines++;
				}
				if (page) pages.push(page);
				for (const [index, page] of pages.entries()) {
					const result = await options.show({ type: "question", title: zh ? "檢視固定操作計畫" : "Review fixed action plan", message: `${label} (${index + 1}/${pages.length})`, detail: page,
						buttons: zh ? ["取消", "繼續檢視"] : ["Cancel", "Continue review"], defaultId: 0, cancelId: 0, noLink: true, signal });
					if (result.response !== 1 || signal.aborted) { await call({ type: "cancel", requestRef: review.requestRef }); return; }
				}
				if (Date.parse(review.plan.validUntil) <= Date.now()) throw new Error("OPERATOR_ACTION_UNAVAILABLE");
				const result = await options.show({ type: "question", title: zh ? "確認本機操作" : "Confirm local action", message: label,
					detail: `${zh ? review.explanation.riskZh : review.explanation.riskEn}\n\n${review.plan.purpose}\n${zh ? "計畫 digest" : "Plan digest"}: ${review.digest}\n${zh ? "有效期限" : "Valid until"}: ${review.plan.validUntil}\n\n${zh ? "此確認僅隔離 AO API 與 session grant。具相同 OS 帳號 exec／讀檔權限、XTEST 或 OS 注入能力的程式仍可能操作確認窗；這不證明物理人類身分。" : "This isolates AO APIs and session grants. Programs with same-account exec/file access, XTEST or OS injection may operate this dialog; this does not prove physical human identity."}`,
					buttons: zh ? ["取消", "確認此計畫"] : ["Cancel", "Confirm this plan"], defaultId: 0, cancelId: 0, noLink: true, signal });
				if (signal.aborted) return;
				if (result.response !== 1) { await call({ type: "cancel", requestRef: review.requestRef }); return; }
				const receipt = await call({ type: "confirm", requestRef: review.requestRef, challenge: review.challenge, digest: review.digest });
				if (receipt.kind !== "confirmed") throw new Error("OPERATOR_CONFIRMATION_INVALID");
				if (!disposed && !signal.aborted) await options.show({ type: "info", title: zh ? "本機操作確認" : "Local action confirmation",
					message: zh ? "已記錄此計畫的確認。" : "Confirmation recorded for this plan.", detail: zh ? "這只記錄同意，不會自動執行操作。" : "This records consent; it does not automatically execute the action.", defaultId: 0, cancelId: 0, noLink: true, signal }).catch(() => undefined);
			} catch {
				if (!disposed) await options.show({ type: "info", title: zh ? "本機操作確認" : "Local action confirmation", message: zh ? "目前沒有可確認的計畫，或 native issuer 無法驗證。" : "No confirmable plan is available, or the native issuer could not be qualified.", detail: zh ? "不會自動執行或降級確認。請檢查計畫期限與桌面連線。" : "No action or fallback confirmation occurred. Check plan expiry and the desktop connection." }).catch(() => undefined);
			} finally { busy = false; }
		},
	};
}
