# Working on pmss-fleet-manager

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and the nearby code before changing behavior. Keep these principles practical and proportionate to the task.

## Keep it simple

- Make the smallest clear change that solves the problem. Follow existing patterns and avoid unrelated refactors or speculative abstractions.
- Preserve the lightweight design: one Go binary, SQLite, an embedded vanilla JavaScript UI, and SSH as the tenant user. Do not introduce remote agents or require root.
- Match safeguards to the consequences. Use stronger checks for credentials, destructive actions, and public submissions without burdening routine changes.

## Respect trust boundaries

- Verify SSH host identity. Verify tenant ownership of remote service sockets before sending credentials or service commands; preserve existing checks around new connections and sensitive actions.
- Keep secrets out of logs, errors, browser responses, and committed examples. Treat remote output, API responses, and user input as untrusted; validate and safely encode them where used.
- Bound remote work and parsing with appropriate deadlines, size limits, concurrency, and cancellation. A slow or malformed response should not stall the fleet.

## Integrate carefully

- Verify external API field meanings, units, authentication, and update semantics against current documentation or observed behavior. Do not infer them from names or assume an update preserves omitted fields.
- Keep optional integrations opt-in. Make outbound data and effects clear, and keep unrelated features usable when an integration is disabled or unavailable.
- Contain failures to the affected operation, box, or integration. Distinguish unknown, stale, failed, and successful states; do not turn missing data into a healthy zero.
- Retry only when safe, with bounds and backoff. Check uncertain write outcomes before retrying actions that could duplicate effects.

## Verify the change

- Add focused tests for changed behavior and meaningful failure cases. Use the repository's documented checks and update relevant documentation when behavior changes.
- Report what changed, what was tested, and what remains unverified. Do not claim a live integration or remote action works based only on mocks or local tests.
