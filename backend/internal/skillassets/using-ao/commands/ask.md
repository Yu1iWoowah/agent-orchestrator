# ao ask

Create an explicit question for the user in the session's attention frame. This
is separate from `ao report --needs-input`, which coordinates with the project's
orchestrator. Use `ask` only when an actual user decision is needed.

```text
ao ask create --key <stable-key> --audience user --kind question --payload-file <file>
ao ask list [--state open|history|all] [--before-sequence <n>] [--limit <1-100>]
ao ask get <question-id>
ao ask answer <question-id> --payload-file <answer-file>
ao ask read <question-id> --expected-version <n>
ao ask cancel <question-id> --expected-version <n> --reason <text> [--actor agent|user]
ao ask revise <question-id> --expected-version <n> --payload-file <file>
```

Every command accepts `--session <origin-session-id>` (default `AO_SESSION_ID`)
and `--json`. Payload files are UTF-8 JSON; `-` reads standard input. The CLI is
only an HTTP client. The daemon derives the project from the origin session.
Do not put project, session, actor or delivery metadata inside the payload.

Example `question.json`:

```json
{
  "schemaVersion": 1,
  "title": "Choose a deployment target",
  "explanation": "The build is ready. A target determines who can use it and the rollback impact.",
  "questions": [{
    "key": "target",
    "header": "Target",
    "question": "Where should this build be deployed?",
    "multiSelect": false,
    "options": [
      {"id":"staging","label":"Staging","description":"Validate with internal users first.","recommended":true},
      {"id":"production","label":"Production","description":"Make the build available to all users."}
    ]
  }]
}
```

Provide 1–4 related questions, each with 2–4 options. IDs and keys are stable,
case-sensitive ASCII tokens (1–64 characters, starting with a letter or digit).
Each option needs a short label and description. At most one option can be
recommended, and it must come first; recommendations never preselect an answer.
Do not include an Other option: every question automatically offers Other free
text. `multiSelect` defaults to false. An optional question-level `explanation`
adds context specific to that question.

An option can include `preview: {"format":"text"|"markdown","content":"..."}`.
Preview is read-only, not an executable widget. Markdown supports prose, lists,
quotes, tables and fenced code. Raw HTML, images, embedded resources and unsafe
URLs are rejected. Links are displayed as text without navigation or network
fetches. Never use previews for secrets or instructions that approve a tool.

Limits use Unicode code points: title 120, overall explanation 8000, header 24,
question 2000, per-question explanation 4000, label 80, option description 1000,
preview 8192 and Other answer 4000. The entire create/revise request JSON,
including its command envelope, is at most 128 KiB; answer JSON is at most
32 KiB. Required text must be nonblank. Unknown members,
schema versions, duplicate keys, explicit nulls and invalid types are rejected.

For `--kind authorization`, also provide an `authorization` object with required
`action` (1000), `scope` (2000), `limits` (2000), and 1–4 `evidence` strings
(1000 each). This records a scoped user decision; it does not resolve any native
provider permission request. Use the provider's original approval form for that.

Create retries with the same origin session, `--key` and normalized payload
return the same question ID. A different payload with that key returns 409.
Use `revise` with the current version for an open question; terminal questions
never reopen. A new replacement can use `create --supersedes-id <terminal-id>`.
There is no default expiration. `--expires-at <RFC3339>` sets an explicit
deadline; terminating the origin session also expires its open questions.
Idling or restarting does not close them.

`ao ask answer` is for the human operator or the user-facing answer interface.
Agents must not invoke it to answer their own or another agent's questions,
infer a choice from conversation, or fill in an answer on the user's behalf.
Answers are recorded as human direction and can update `latest_user_prompt`;
this command does not authenticate who is typing on the local daemon socket.
This contract does not authorize agent-transcribed answers. Present the question
and wait for the user to submit through the interface or CLI. Recommendations,
previews and ordinary chat messages never count as an answer or tool approval.

Example `answer.json` (read the current question before answering):

```json
{
  "questionId": "ask_example",
  "expectedVersion": 1,
  "clientAnswerId": "answer-unique-1",
  "answers": [{"questionKey":"target","selectedOptionIds":["staging"]}]
}
```

Send all grouped answers together. A single-select answer chooses exactly one
offered ID or provides `otherText` with `selectedOptionIds: []`. Multi-select can
combine offered IDs and Other; at least one response is required. Other is text,
never a fabricated option ID. `clientAnswerId` is a stable 1–128 character ASCII
token. Keep it across uncertain retries; retrying the same payload returns the
saved receipt and does not enqueue another delivery. Reusing it for a different
answer returns 409. On 409, reload and ask the user to choose from the fresh
version; do not silently map old choices onto a changed question.

Saving answers and closing the question is atomic. `queued` means saved with
delivery still pending; only `delivered` confirms semantic acceptance by the
origin agent. Busy, offline or unsupported delivery backs off from 5 seconds
to at most 5 minutes; restart retries the same saved message immediately and
then resumes backoff. Unsupported acceptance stays queued, never delivered.
An unavailable origin leaves the answer readable with its delivery failure. If a request times
out, use `get` to inspect its saved answer and retry with the same client answer
ID; do not assume cancellation. Ordinary conversation messages do not answer,
cancel or approve a structured question. Read state never changes pending count.
