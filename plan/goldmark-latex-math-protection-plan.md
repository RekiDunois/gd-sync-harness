# Goldmark LaTeX Math Protection Repair Plan

## Problem

The compiler currently parses Markdown with Goldmark plus the wikilink and
hashtag extensions, but without a third-party math extension. As a result,
math expressions containing `[[...]]` can be collected as wikilinks. Examples
include matrix-like expressions inside `$...$` or `$$...$$`.

The source Markdown must remain unchanged. The repair concerns only parser AST
classification and derived link/index artifacts.

## Goal

Use a third-party Goldmark math extension to make math regions opaque to the
wikilink and hashtag collectors, while preserving real links outside math
regions.

## Candidate Library

Evaluate `github.com/aziis98/goldmark-latex` first. It provides Goldmark math
AST nodes and configurable delimiters. Consider
`github.com/litao91/goldmark-mathjax` only as a fallback because it is older
and has fewer delimiter options.

Do not use a Markdown-to-LaTeX renderer as a math-region parser.

## Implementation Steps

1. Add the selected third-party dependency with a pinned version.
2. Register the math extension in both parser instances in
   `internal/compiler/parser.go`.
3. Configure the delimiters used by the corpus:
   - inline: `$...$` and, if supported, `\(...\)`;
   - block: `$$...$$` and, if supported, `\[...\]`.
4. Keep parser-owned AST extraction. Do not add a second regex lexer or mutate
   source text before parsing.
5. Add characterization tests for math blocks, inline math, code spans,
   fenced code, escaped delimiters, and real wikilinks outside math.
6. Verify that math AST subtrees do not emit `wikilink.Node` or hashtag facts.
7. Recompile a local immutable generation and compare link counts, broken-link
   categories, warnings, and manifest hashes.
8. Run the full test suite and local integrity verification.
9. Publish the repaired generation through DerivedSync only after all checks
   pass.

## Acceptance Criteria

- `[[...]]` inside every supported math delimiter produces no local-link edge.
- `[[real-note]]` outside math still produces one wikilink occurrence.
- `![[image.png]]` outside math remains an embed edge.
- Code spans and fenced code do not produce math or wikilink facts unless the
  accepted parser behavior explicitly requires it.
- Source file bytes and content hashes are unchanged by compilation.
- The repaired generation passes `compiler status --verify`.
- `go test ./...`, `go vet ./...`, and `git diff --check` pass.
- DerivedSync reaches `current` only after detail sync, full check, and final
  `MANIFEST.json` commit succeed.

## Non-Goals

- Do not repair or rewrite existing source notes in this change.
- Do not infer whether a malformed source fragment was intended as math.
- Do not resolve links outside the eligible source root.
- Do not sanitize or redesign the broader broken-link report in this repair;
  track that as a separate artifact-safety task.

## Failure Handling

- If the selected library cannot coexist with the pinned Goldmark and wikilink
  extension, stop before changing the production parser.
- If characterization tests expose behavior differences, record the exact
  fixture and evaluate another compatible third-party extension.
- If the repaired generation fails local verification or remote publication,
  retain the last verified generation and do not advance the remote commit
  marker.
