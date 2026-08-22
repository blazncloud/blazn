# Anthropic Messages adapter qualification

The checked `claude-code-2.1.212-harness-shape.json` fixture is a reproducible
compatibility harness targeting the request shape expected from Claude Code
2.1.212. It is not a byte-for-byte capture from the pinned client. Exact client
capture and endpoint qualification remain pending and must not be inferred from
the package tests.

The POC adapter supports native Anthropic Messages request, response, and SSE
translation through metadata-aware adapter methods. In particular, the matched
`stop_reason` and `stop_sequence` travel alongside the frozen normalized
envelope and are restored at the Anthropic source boundary.

The frozen OpenAI Chat and Responses routes intentionally reject Anthropic
streaming and requests with stop sequences. Those routes cannot preserve
Anthropic input-usage timing or the identity of the matched stop sequence, so a
cross-protocol attempt fails before route commitment instead of silently losing
information.
