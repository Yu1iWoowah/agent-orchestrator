import { useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { ConversationFrames } from "../../src/renderer/components/chat/ConversationFrames";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import { Button } from "../../src/renderer/components/ui/button";
import { useConversationFrameSafeRect } from "../../src/renderer/hooks/useConversationFrameSafeRect";
import "../../src/renderer/styles.css";

// Shared production chrome with deterministic content; no attention business API.
const params = new URLSearchParams(location.search);
const locale = params.get("locale") === "zh-TW" ? "zh-TW" : "en";
const i18n = createFullAppI18n(locale);
const title = locale === "zh-TW" ? "待你回覆" : "Awaiting you";
function Harness() {
	const surface = useRef<HTMLElement>(null);
	const composer = useRef<HTMLElement>(null);
	const bounds = useConversationFrameSafeRect(surface, composer);
	const [queue, setQueue] = useState(false);
	return (
		<section
			ref={surface}
			className="relative flex h-screen min-h-0 flex-col bg-background text-foreground"
		>
			<header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
				<span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
					Renderer fixture · shared frame chrome · PR 1
				</span>
				<Button size="sm" variant="ghost" onClick={() => setQueue((value) => !value)}>
					Composer queue
				</Button>
				{bounds ? (
					<ConversationFrames
						sessionId={params.get("session") ?? "frame-fixture"}
						surface={bounds.surface}
						safeRect={bounds.rect}
						frames={[
							{
								kind: "attention",
								title,
								summary: "",
								content: (
									<div className="space-y-3 text-sm">
										<p>Shared frame fixture. Live questions connect in PR 2.</p>
										<label className="block">
											Frame draft
											<input
												className="mt-2 w-full rounded-md border border-input p-2"
												aria-label="Frame draft"
												defaultValue="Keep this text when resizing"
											/>
										</label>
									</div>
								),
							},
							{
								kind: "plan",
								title: "Plan",
								summary: "1/4",
								content: (
									<ol className="list-inside list-decimal space-y-3 text-sm">
										<li className="text-muted-foreground line-through">Read the requirements</li>
										<li>Implement the session Plan frame</li>
										<li>Verify composer clearance and independent slots</li>
										<li>Open the scoped PR for review</li>
									</ol>
								),
							},
						]}
					/>
				) : null}
			</header>
			<div className="chat-scroll-viewport min-h-0 flex-1 overflow-auto px-4 py-5">
				<article className="mx-auto max-w-3xl space-y-7 text-sm leading-7">
					{Array.from({ length: 18 }, (_, index) => (
						<p key={index}>
							Conversation content {index + 1}. The centered reading column keeps its 48rem cap
							while the independent frames overlay the right side. Use each frame's own collapse,
							resize and position controls.
						</p>
					))}
				</article>
			</div>
			<footer ref={composer} className="cursor-chat-composer-dock shrink-0 px-4 pb-4">
				<div className="mx-auto max-w-3xl rounded-xl border bg-card p-3">
					{queue ? (
						<div className="mb-3 h-28 border-b text-sm">
							Queued message · fixture.txt attachment
						</div>
					) : null}
					<textarea
						aria-label="Conversation draft"
						className="h-20 w-full resize-none bg-transparent text-sm outline-none"
						placeholder="Composer remains clear"
					/>
				</div>
			</footer>
		</section>
	);
}
const root = document.getElementById("frame-root");
if (root)
	createRoot(root).render(
		<I18nextProvider i18n={i18n}>
			<TooltipProvider>
				<Harness />
			</TooltipProvider>
		</I18nextProvider>,
	);
