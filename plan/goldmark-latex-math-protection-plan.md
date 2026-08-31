# Goldmark Math-Region Protection Repair Plan

## Problem

The compiler currently parses Markdown with Goldmark plus the wikilink and
hashtag extensions, but it has no parser-level ownership for math regions. As a
result, Markdown-looking text inside LaTeX/MathJax expressions can be parsed as
normal Markdown facts. In particular, `[[...]]` inside `$...$` or `$$...$$`
can become a local-link edge, and `#...` can become a hashtag fact.

The source Markdown must remain unchanged. The repair concerns only parser AST
classification and the derived structural facts collected from that AST.

The dangerous failure mode is symmetric:

- under-protecting math creates false-positive links/tags from formula text;
- over-protecting prose creates false-negative real links/tags by swallowing
  ordinary Markdown into an incorrectly claimed math region.

Both are correctness failures for the deterministic knowledge graph.

## Goal

Make supported Obsidian-style math regions opaque to the compiler's wikilink
and hashtag collectors while preserving real Markdown facts outside math.

The implementation must stay inside the existing Goldmark parse pipeline:

```text
source bytes
  -> one Goldmark parser pipeline
     -> math-region parser(s)
     -> wikilink / hashtag / normal Markdown parsers
  -> one AST
  -> existing structural collector
```

Do not add a regex pre-pass, source mutation, a second Markdown lexer, or a
post-parse raw-text rescanner.

## Syntax Contract

V1 protects the math syntax documented by Obsidian itself:

- inline math: `$...$`;
- display math: `$$...$$`, including the normal standalone multiline form.

Obsidian reference:

- [Obsidian Help: Advanced formatting syntax — Math](https://github.com/obsidianmd/obsidian-help/blob/master/en/Editing%20and%20formatting/Advanced%20formatting%20syntax.md#math)

Do **not** include `\(...\)` or `\[...\]` in V1 merely because MathJax can
recognize them. They are not part of the currently documented Obsidian Markdown
math surface. Add them only in a later change if corpus evidence requires them
and a separate characterization matrix defines their Markdown interaction.

### Inline `$...$` contract

Use the Pandoc/Vale-style currency guard as the baseline delimiter rule:

1. the opening `$` must be followed by a non-whitespace character;
2. the closing `$` must be preceded by a non-whitespace character;
3. the closing `$` must not be immediately followed by an ASCII decimal digit;
4. escaped dollars must not accidentally become closing delimiters;
5. if no valid closer exists before the accepted inline-math boundary, the
   parser must consume nothing and ordinary Markdown parsing must continue.

The exact multiline-inline boundary and escape behavior must be pinned by tests
before production registration. The parser must never greedily claim a failed
candidate and then rely on a later compensation pass to recover real Markdown.

### Display `$$...$$` contract

The display parser must be conservative:

1. only claim an accepted Obsidian display-math opening shape;
2. confirm that an accepted closing `$$` exists before committing to an opaque
   block/span;
3. if no valid closer exists, consume nothing and let ordinary Markdown parse
   the source;
4. once claimed, the math body is raw/opaque and has no wikilink/hashtag
   descendants;
5. arbitrary `$$` appearing in the middle of ordinary prose must not silently
   become a file-consuming block.

A malformed or unclosed display candidate must therefore fail open to normal
Markdown rather than hiding the rest of the document.

## Research Conclusion

The original plan assumed that the repair should select an existing third-party
Goldmark math extension. The library audit does not support that assumption.

No reviewed off-the-shelf dependency simultaneously provides all of the
following:

- compatibility with the project's current Goldmark generation without a
  parser upgrade being required solely for this fix;
- lightweight parser-only use without a MathJax/KaTeX/JavaScript rendering
  runtime;
- currency-safe `$...$` delimiter semantics;
- safe rollback for malformed/unclosed candidates;
- opaque AST ownership suitable for suppressing nested wikilink/hashtag facts;
- behavior close enough to the documented Obsidian math surface.

Several mature Go projects independently reached the same design: they use a
small project-owned Goldmark math parser rather than delegating delimiter
correctness to an old renderer-oriented extension.

The default implementation choice for this repair is therefore a **minimal
project-owned Goldmark math-region extension**. It remains part of Goldmark's
single parser/AST pipeline and is not the prohibited "second Markdown lexer."

Before implementation, explicitly reconcile this decision with the locked
Knowledge Compiler architecture statement that syntax facts come from
third-party parser/extensions. The amended architectural rule should preserve
the real invariant:

> Goldmark remains the sole Markdown parser and the compiler consumes only
> structured AST results. A narrowly scoped project-owned Goldmark math-region
> extension is permitted when characterized third-party extensions cannot meet
> the required syntax contract. Raw-text fallback scanners and a second
> Markdown lexer remain prohibited.

## Reference Implementations

These are behavior and implementation references, not dependencies that should
be copied wholesale. If code is copied rather than independently implemented,
review and preserve the applicable upstream license/attribution requirements.

### 1. Vale — primary delimiter-correctness reference

Vale replaced the old math-extension behavior with project-owned Goldmark
parsers specifically because naive `$` pairing hid currency text and incorrect
`$$` closing behavior could consume the rest of a document.

- implementation:
  [vale-cli/vale `internal/lint/math.go`](https://github.com/vale-cli/vale/blob/v3/internal/lint/math.go)
- characterization tests:
  [vale-cli/vale `internal/lint/math_test.go`](https://github.com/vale-cli/vale/blob/v3/internal/lint/math_test.go)
- production failure that motivated display-math hardening:
  [vale-cli/vale issue #1148](https://github.com/vale-cli/vale/issues/1148)

Borrow conceptually:

- Pandoc-style single-dollar opening/closing guards;
- escaped-dollar handling;
- reader-position rollback on an unclosed inline candidate;
- regression fixtures for currency, code spans, multiline inline math,
  one-line display math, escaped closers, and adjacent display blocks.

Do not automatically copy Vale's exact unclosed-display policy. Vale bounds an
unclosed display block at a blank line for linting purposes; the compiler should
prefer not to claim an unclosed display region at all.

### 2. `goldmark-mathml` — display preflight / fail-open reference

This extension implements GitHub math semantics rather than Obsidian semantics,
so it is not the production dependency. Its block parser is nevertheless a
strong reference for correctness: it looks ahead for a valid display closer
before claiming a block and leaves unclosed candidates as literal Markdown.

- block parser:
  [`filippo-agent/goldmark-mathml` `block.go`](https://github.com/filippo-agent/goldmark-mathml/blob/main/block.go)
- inline parser:
  [`filippo-agent/goldmark-mathml` `inline.go`](https://github.com/filippo-agent/goldmark-mathml/blob/main/inline.go)
- extension wiring/priorities:
  [`filippo-agent/goldmark-mathml` `extension.go`](https://github.com/filippo-agent/goldmark-mathml/blob/main/extension.go)

Borrow conceptually:

- preflight an ambiguous block before taking parser ownership;
- failed candidates consume nothing;
- math nodes are structural parser results rather than post-parse source scans.

Do not inherit its GitHub-specific `$` rules or its Goja/Temml rendering
runtime.

### 3. Memos — compact upstream-Goldmark parser reference

Memos has a small dedicated Goldmark math parser with explicit currency tests
and reader rollback.

- implementation:
  [usememos/memos `internal/markdown/parser/math.go`](https://github.com/usememos/memos/blob/main/internal/markdown/parser/math.go)
- tests:
  [usememos/memos `internal/markdown/parser/math_test.go`](https://github.com/usememos/memos/blob/main/internal/markdown/parser/math_test.go)

Borrow conceptually:

- keep the delimiter parser small and parser-local;
- test currency lists and retry after rejected candidate closers;
- restore reader position when a candidate does not close.

Its current syntax contract is not adopted automatically; use it as an
implementation cross-check only.

### 4. Logseq Go — note-application semantics reference

`logseq-go` implements math directly as Goldmark AST nodes and documents the
currency motivation for its single-dollar rules.

- implementation:
  [aholstenson/logseq-go `internal/markdown/math.go`](https://github.com/aholstenson/logseq-go/blob/main/internal/markdown/math.go)

Borrow conceptually:

- separate single-dollar inline behavior from double-dollar display behavior;
- keep math body opaque instead of recursively parsing Markdown inside it;
- use the note application's Markdown semantics, not renderer convenience, as
  the delimiter contract.

### 5. mdsmith — detection-only AST design reference

mdsmith's math extensions are explicitly detection-only and expose raw math AST
nodes rather than coupling detection to rendering. It uses its own Goldmark
fork, so its package cannot be imported into the current compiler, but the node
shape is directly relevant.

- inline parser:
  [jeduden/mdsmith `mathinline.go`](https://github.com/jeduden/mdsmith/blob/main/pkg/markdown/flavor/ext/mathinline.go)
- block parser/node:
  [jeduden/mdsmith `mathblock.go`](https://github.com/jeduden/mdsmith/blob/main/pkg/markdown/flavor/ext/mathblock.go)

Borrow conceptually:

- detection-only extension;
- dedicated `MathInline` / `MathBlock` node kinds;
- raw block nodes with no parsed Markdown children;
- parser state belongs to the node/candidate, not a second parsing layer.

### 6. Hugo passthrough — opaque-region mechanism reference

Hugo's passthrough extension consumes a matched delimited region in one parser
operation specifically so other Goldmark extensions cannot operate on its
contents, and it restores the reader when a closer is missing.

- implementation:
  [Hugo passthrough v0.3.1 `passthrough.go`](https://github.com/gohugoio/hugo-goldmark-extensions/blob/passthrough/v0.3.1/passthrough/passthrough.go)

Borrow conceptually:

- claim the entire protected region as one opaque AST node;
- do not expose interior text to wikilink/hashtag parsing;
- restore/fail open on an unclosed candidate.

Do not use generic passthrough directly for `$...$`: it is a delimiter matcher,
not a semantic math parser, so ordinary currency such as `$5 ... $10` can
incorrectly hide real Markdown between the dollars.

### 7. Additional comparison implementations

Useful secondary references for independently implemented Goldmark math-region
parsers:

- [AO Cyber Systems / eden-press `press/math/math.go`](https://github.com/AO-Cyber-Systems/eden-press/blob/main/press/math/math.go)
- [boxesandglue/glu `markdown/mathext/mathext.go`](https://github.com/boxesandglue/glu/blob/main/markdown/mathext/mathext.go)
- [graemephi/goldmark-qjs-katex `qjskatex.go`](https://github.com/graemephi/goldmark-qjs-katex/blob/master/qjskatex.go)

These reinforce the same overall pattern: delimiter ownership is small enough to
implement as a Goldmark parser, while rendering libraries add dependencies and
semantics the compiler does not need.

## Rejected Production Dependencies

### `github.com/aziis98/goldmark-latex`

Reject as the production choice. It is old, based on much older Goldmark
internals, has limited delimiter configuration, and its inline failure behavior
is not strong enough to make unclosed candidates an acceptable correctness
risk.

### `github.com/litao91/goldmark-mathjax`

Reject as the production choice. It is renderer-oriented and old, lacks the
required currency guard, and its block behavior has the same class of
file-swallowing failure later fixed independently by projects such as Vale.

### `github.com/powerman/goldmark-obsidian`

Do not treat the repository name as proof of math-parser fidelity. Its Obsidian
extension currently delegates math parsing to `litao91/goldmark-mathjax`, so it
inherits the relevant limitations.

### `github.com/gohugoio/hugo-goldmark-extensions/passthrough`

Do not use directly for single-dollar math because generic delimiter pairing can
turn currency into an opaque region and hide legitimate wikilinks/tags.

### `github.com/filippo-agent/goldmark-mathml`

Do not use as the production dependency for this compiler repair. Its parser is
high-quality but deliberately targets GitHub math semantics and the module also
brings a MathML rendering stack (`goja`/Temml) that the compiler does not need.

### KaTeX / MathJax rendering extensions generally

Do not introduce QuickJS, Goja, KaTeX, MathJax, MathML rendering, CSS/font
assets, or server-side TeX conversion merely to classify Markdown math regions.
The compiler needs syntax ownership, not rendering.

## Proposed Implementation

Add a minimal internal Goldmark extension dedicated to math-region protection.
It should contain only:

- an inline parser for accepted `$...$` regions;
- a display parser for accepted `$$...$$` regions;
- opaque AST node kinds such as `MathInline` and `MathBlock`;
- no renderer;
- no LaTeX parser;
- no source rewriting;
- no secondary source scanner.

Prefer nodes that are leaves/raw from the collector's perspective. The existing
collector should continue to walk the one Goldmark AST and simply find no
wikilink/hashtag descendants inside a math node.

Register exactly the same math extension configuration in both parser instances
in `internal/compiler/parser.go`. Use one shared parser-construction helper so
frontmatter/no-frontmatter paths cannot drift.

The math extension must coexist with the current pinned versions of Goldmark,
`go.abhg.dev/goldmark/wikilink`, and `go.abhg.dev/goldmark/hashtag` before any
production parser change is accepted.

## Characterization Gate

Before production registration, implement a blocking characterization suite.
The suite must assert both final `Syntax.Links` / `Syntax.Tags` results and, for
critical cases, AST shape.

At minimum include these synthetic fixtures:

| Fixture | Expected structural result |
| --- | --- |
| `$[[fake]]$` | no wikilink |
| `$x #fake [[fake]]$` | no wikilink, no hashtag |
| `$$[[fake]]$$` in an accepted standalone display form | no wikilink |
| multiline `$$ ... [[fake]] ... $$` | no wikilink/tag from body |
| `before [[real]] $[[fake]]$ after` | exactly one real wikilink |
| `![[image.png]] $![[fake.png]]$` | real embed remains; math embed suppressed |
| `pay $5 and [[real]] for $10` | `[[real]]` must remain visible |
| `$20 and $30 then [[real]] and $x$` | currency does not swallow the real link; valid later math is protected |
| unclosed `$[[text]]` followed by `[[real]]` | failed math candidate does not swallow `[[real]]` |
| unclosed `$$` followed by ordinary Markdown | no opaque display node may consume unrelated following Markdown |
| `` `$[[fake]]$` `` | code span owns its contents; no wikilink/math fact from the code contents |
| fenced code containing `$[[fake]]$` | no math or wikilink fact from fenced code |
| escaped dollars around prose | no accidental math ownership |
| two adjacent display blocks | both terminate independently; following Markdown remains visible |
| math inside a blockquote/list item | accepted math body remains opaque without corrupting container structure |
| malformed frontmatter followed by math + real link | existing frontmatter warning remains; fake math fact suppressed; real link emitted |

Add explicit AST assertions for representative inline and display cases:

- a successful math node has no `wikilink.Node` descendant;
- a successful math node has no hashtag-node descendant;
- a rejected/unclosed candidate does not leave a partial math node;
- code-span/fenced-code ownership still prevents nested math parsing;
- real Markdown after a rejected candidate remains in the normal AST.

If any candidate parser implementation cannot satisfy the complete matrix
without a second source scan or a compensating post-pass, reject that
implementation rather than weakening the fixtures.

## Implementation Steps

1. Reconcile the Knowledge Compiler architecture text with the explicitly
   reviewed project-owned Goldmark extension exception described above.
2. Add the blocking characterization tests first, including currency,
   unclosed-candidate rollback, AST opacity, and parser-composition cases.
3. Implement the minimal parser-only math protection extension using upstream
   Goldmark APIs compatible with the project's pinned Goldmark version.
4. Register it through one shared parser-construction helper used by both the
   `markdown` and `plain` parser instances in `internal/compiler/parser.go`.
5. Keep `collect` AST-only. Do not add special raw-source suppression logic to
   the collector.
6. Verify the exact parser priorities and AST ownership against code spans,
   fenced code, wikilinks, hashtags, links, tables, and frontmatter paths.
7. Run the focused compiler/parser characterization suite.
8. Run `go test ./...`, `go vet ./...`, and `git diff --check`.
9. Recompile a synthetic/local immutable generation and compare structural
   metrics: link counts, broken-link categories, warnings, and manifest hashes.
10. Verify source file bytes/content hashes are unchanged by compilation.
11. Publish through DerivedSync only after local compiler verification passes;
    do not advance the remote current marker on a failed verification/publication.

## Acceptance Criteria

- `[[...]]`, embeds, and hashtags inside every V1-supported math form produce no
  corresponding compiler structural facts.
- Real wikilinks/embeds/hashtags immediately before or after math remain visible.
- Currency/prose dollar signs do not hide real Markdown facts.
- Malformed or unclosed math candidates cannot consume unrelated following
  Markdown.
- Code spans and fenced code retain ownership of their contents and do not
  generate nested math/wikilink/hashtag facts.
- Both compiler parser instances use exactly the same math-region configuration.
- The collector remains AST-only; no raw-text fallback or second Markdown lexer
  is introduced.
- Source file bytes and content hashes are unchanged by compilation.
- The repaired generation passes `compiler status --verify`.
- `go test ./...`, `go vet ./...`, and `git diff --check` pass.
- DerivedSync reaches `current` only after detail sync, full check, and final
  `MANIFEST.json` commit succeed.

## Non-Goals

- Do not render LaTeX, MathJax, KaTeX, or MathML.
- Do not validate whether the contents of a protected region are valid LaTeX.
- Do not repair or rewrite existing source notes.
- Do not infer intent for malformed dollar syntax beyond the explicitly
  characterized delimiter contract.
- Do not add `\(...\)` or `\[...\]` support in V1 without a separate evidence
  and characterization change.
- Do not resolve links outside the eligible source root.
- Do not redesign the broader broken-link report in this repair; track that as
  a separate artifact-safety task.

## Failure Handling

- If the project-owned parser cannot coexist with the pinned Goldmark/wikilink/
  hashtag stack, stop before changing production parsing behavior and record the
  smallest failing fixture.
- If a third-party parser discovered later passes the entire characterization
  gate with a smaller and cleaner dependency surface, it may replace the
  project-owned implementation after explicit review; passing the gate, not the
  package name, is the acceptance criterion.
- If an ambiguous delimiter case is not covered by the frozen syntax contract,
  fail conservatively and add a fixture before broadening parser ownership.
- If the repaired generation fails local verification or remote publication,
  retain the last verified generation and do not advance the remote commit
  marker.
