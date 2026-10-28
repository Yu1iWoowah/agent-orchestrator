import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import { Button } from "../../src/renderer/components/ui/button";
import "../../src/renderer/styles.css";

// This dev-only fixture never reaches a daemon, provider or stable API.
const originalFetch = window.fetch.bind(window);
window.fetch = async (input, init) => {
  const url = new URL(
    input instanceof Request ? input.url : String(input),
    location.href,
  );
  if (!url.pathname.startsWith("/api/")) return originalFetch(input, init);
  const history = url.searchParams.get("state") === "history";
  if (url.pathname.endsWith("/attention")) {
    return Response.json({
      conversationId: "fixture",
      activeBranchId: "root",
      counts: { open: 3, unread: 3, history: 1 },
      hasMoreBefore: false,
      items: (history ? [4] : [3, 2, 1]).map((i) => ({
        branchId: "root",
        actionable: !history,
        read: history,
        activity: {
          kind: "activity",
          id: `fixture-${i}`,
          requestId: `fixture-rpc-${i}`,
          activityKind: i === 2 ? "approval" : "user_input",
          status: history ? "resolved" : "pending",
          sequence: i,
          revision: 0,
          summary: history
            ? "Previously answered fixture"
            : `Fixture request ${i}`,
          createdAt: "2026-10-29T00:00:00Z",
          detail: {
            message: history
              ? "Previously answered fixture"
              : `Fixture request ${i}: review the isolated deployment environment and explain its impact.`,
          },
        },
      })),
    });
  }
  if (url.pathname.endsWith("/plan"))
    return Response.json({
      state: "absent",
      conversationId: "fixture",
      activeBranchId: "root",
    });
  return Response.json(
    { code: "NOT_FOUND", message: "No fixture for this API" },
    { status: 404 },
  );
};
// Import after installing the boundary so the generated client captures it.
const { SessionConversationFrames } =
  await import("../../src/renderer/components/chat/SessionConversationFrames");
const { setApiBaseUrl } = await import("../../src/renderer/lib/api-client");
setApiBaseUrl(location.origin);
const params = new URLSearchParams(location.search);
const i18n = createFullAppI18n(
  params.get("locale") === "zh-TW" ? "zh-TW" : "en",
);
function Harness() {
  const surface = useRef<HTMLElement>(null),
    composer = useRef<HTMLElement>(null);
  const [queue, setQueue] = useState(false),
    [located, setLocated] = useState("");
  return (
    <section
      ref={surface}
      className="relative flex h-screen min-h-0 flex-col bg-background text-foreground"
    >
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
        <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
          PR 2 renderer fixture · no daemon or provider
        </span>
        <Button variant="ghost" size="sm" onClick={() => setQueue((v) => !v)}>
          Composer queue
        </Button>
        <SessionConversationFrames
          sessionId={params.get("session") ?? "attention-fixture"}
          activeBranchId="root"
          surfaceRef={surface}
          composerRef={composer}
          onLocateAttention={(id) => setLocated(id)}
        />
      </header>
      <article className="chat-scroll-viewport min-h-0 flex-1 overflow-auto p-6">
        <div className="mx-auto max-w-3xl space-y-6 text-sm">
          <p>
            Deterministic renderer fixture. Source navigation and provider
            submission are verified separately in the real app and behavioral
            tests.
          </p>
          <p role="status">
            {located
              ? `Located ${located} (fixture only)`
              : "No fixture selected"}
          </p>
          {Array.from({ length: 12 }, (_, i) => (
            <p key={i}>
              Conversation content {i + 1}. The two independent frames overlay
              the reading area without moving the composer.
            </p>
          ))}
        </div>
      </article>
      <footer
        ref={composer}
        className="cursor-chat-composer-dock shrink-0 px-4 pb-4"
      >
        <div className="mx-auto max-w-3xl rounded-xl border bg-card p-3">
          {queue ? (
            <div className="mb-3 h-28 border-b">
              Queued message · fixture.txt
            </div>
          ) : null}
          <textarea
            aria-label="Conversation draft"
            className="h-20 w-full resize-none bg-transparent outline-none"
            placeholder="Composer remains clear"
          />
        </div>
      </footer>
    </section>
  );
}
const root = document.getElementById("attention-root");
if (!root) throw new Error("Missing fixture root");
createRoot(root).render(
  <QueryClientProvider client={new QueryClient()}>
    <I18nextProvider i18n={i18n}>
      <TooltipProvider>
        <Harness />
      </TooltipProvider>
    </I18nextProvider>
  </QueryClientProvider>,
);
