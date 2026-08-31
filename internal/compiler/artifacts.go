package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type frontmatterStats struct {
	SchemaVersion        int                         `json:"schema_version"`
	CompileGenerationID  string                      `json:"compile_generation_id"`
	NotesTotal           int                         `json:"notes_total"`
	NotesWithFrontmatter int                         `json:"notes_with_frontmatter"`
	Fields               map[string]FrontmatterField `json:"fields"`
}

func serialize(input Input, contractHash string, model *graphModel) (*BuildResult, error) {
	artifacts := map[string][]byte{}
	var err error
	artifacts["FILE_INDEX.jsonl"], err = marshalJSONL(fileRecords(model.Files))
	if err != nil {
		return nil, err
	}
	links := make([]any, 0, len(model.Links))
	for _, value := range model.Links {
		value.CompileGenerationID = input.GenerationID
		links = append(links, value)
	}
	artifacts["LINKS.jsonl"], err = marshalJSONL(links)
	if err != nil {
		return nil, err
	}
	orphans := make([]any, 0, len(model.Orphans))
	for _, value := range model.Orphans {
		value.CompileGenerationID = input.GenerationID
		orphans = append(orphans, value)
	}
	artifacts["ORPHANS.jsonl"], err = marshalJSONL(orphans)
	if err != nil {
		return nil, err
	}
	broken := make([]any, 0, len(model.Broken))
	for _, value := range model.Broken {
		value.CompileGenerationID = input.GenerationID
		broken = append(broken, value)
	}
	artifacts["BROKEN_LINKS.jsonl"], err = marshalJSONL(broken)
	if err != nil {
		return nil, err
	}

	tags := make([]any, 0, len(model.Tags))
	for tagName, value := range model.Tags {
		tags = append(tags, TagRecord{
			SchemaVersion: SchemaVersion, CompileGenerationID: input.GenerationID,
			Tag: tagName, NoteCount: len(value.Notes), OccurrenceCount: value.Count,
			Variants: sortedSet(value.Variants), NotePaths: sortedSet(value.Paths),
		})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].(TagRecord).Tag < tags[j].(TagRecord).Tag })
	artifacts["TAG_INDEX.jsonl"], err = marshalJSONL(tags)
	if err != nil {
		return nil, err
	}

	stats := frontmatterStats{
		SchemaVersion: SchemaVersion, CompileGenerationID: input.GenerationID,
		NotesTotal: len(model.Notes), Fields: map[string]FrontmatterField{},
	}
	for name, field := range model.Frontmatter {
		copyField := *field
		copyField.TypeCounts = cloneIntMap(field.TypeCounts)
		stats.Fields[name] = copyField
	}
	for _, note := range model.Notes {
		if note.Syntax.FrontmatterPresent {
			stats.NotesWithFrontmatter++
		}
	}
	artifacts["FRONTMATTER_STATS.json"], err = json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return nil, err
	}
	artifacts["FRONTMATTER_STATS.json"] = append(artifacts["FRONTMATTER_STATS.json"], '\n')

	for i := range model.Warnings {
		model.Warnings[i].CompileGenerationID = input.GenerationID
		model.Warnings[i].SchemaVersion = SchemaVersion
	}
	sort.Slice(model.Warnings, func(i, j int) bool {
		if model.Warnings[i].Path != model.Warnings[j].Path {
			return model.Warnings[i].Path < model.Warnings[j].Path
		}
		if model.Warnings[i].Code != model.Warnings[j].Code {
			return model.Warnings[i].Code < model.Warnings[j].Code
		}
		return model.Warnings[i].Message < model.Warnings[j].Message
	})
	warnings := make([]any, 0, len(model.Warnings))
	for _, warning := range model.Warnings {
		warnings = append(warnings, warning)
	}
	artifacts["WARNINGS.jsonl"], err = marshalJSONL(warnings)
	if err != nil {
		return nil, err
	}

	counts := counts(model)
	artifacts["ORPHANS.md"] = []byte(renderOrphans(input, model))
	artifacts["BROKEN_LINKS.md"] = []byte(renderBroken(input, model))
	artifacts["KNOWLEDGE_HEALTH.md"] = []byte(renderHealth(input, model, counts, contractHash))
	artifacts["README.md"] = []byte(renderREADME(input))

	return &BuildResult{Manifest: Manifest{
		SchemaVersion: SchemaVersion, CompilerVersion: CompilerVersion,
		ProfileID: input.ProfileID, ProfileUUID: input.ProfileUUID,
		CompilerRunID: input.CompilerRunID, CompileGenerationID: input.GenerationID,
		CompiledAt: input.CompiledAt, PolicyHash: input.PolicyHash,
		EligibilityContractHash: contractHash, Counts: counts,
	}, Artifacts: artifacts}, nil
}

func fileRecords(values []FileRecord) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func marshalJSONL(values []any) ([]byte, error) {
	var out []byte
	for _, value := range values {
		line, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, nil
}

func artifactInventory(artifacts map[string][]byte) map[string]ArtifactInfo {
	keys := make([]string, 0, len(artifacts))
	for key := range artifacts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]ArtifactInfo, len(keys))
	for _, key := range keys {
		sum := sha256.Sum256(artifacts[key])
		info := ArtifactInfo{SHA256: hex.EncodeToString(sum[:]), Bytes: len(artifacts[key])}
		if strings.HasSuffix(key, ".jsonl") {
			info.Records = countLines(artifacts[key])
		}
		out[key] = info
	}
	return out
}

func countLines(value []byte) int {
	if len(value) == 0 {
		return 0
	}
	return strings.Count(string(value), "\n")
}

func counts(model *graphModel) AggregateCounts {
	value := AggregateCounts{EligibleFiles: len(model.Files), Notes: len(model.Notes), Warnings: len(model.Warnings), Tags: len(model.Tags), BrokenLinks: len(model.Broken)}
	value.Links = len(model.Links)
	for _, orphan := range model.Orphans {
		if orphan.HardOrphan {
			value.HardOrphans++
		}
		if orphan.NoBacklink {
			value.NoBacklinkNotes++
		}
		if orphan.OutboundOnly {
			value.OutboundOnlyNotes++
		}
	}
	value.Attachments = value.EligibleFiles - value.Notes
	return value
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func cloneIntMap(values map[string]int) map[string]int {
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func renderREADME(input Input) string {
	return fmt.Sprintf("# Knowledge Compiler\n\nThis directory is managed by knowledge-sync; do not edit it manually.\n\n- Generation: `%s`\n- Consistency authority: `MANIFEST.json`\n- Structure overview: `KNOWLEDGE_HEALTH.md`\n- Orphans: `ORPHANS.md`\n- Broken links: `BROKEN_LINKS.md`\n- Exact indexes: `FILE_INDEX.jsonl`, `LINKS.jsonl`, `TAG_INDEX.jsonl`\n- Metadata and warnings: `FRONTMATTER_STATS.json`, `WARNINGS.jsonl`\n", input.GenerationID)
}

func renderHealth(input Input, model *graphModel, c AggregateCounts, contractHash string) string {
	return fmt.Sprintf("# Knowledge Health\n\n- Generation: `%s`\n- Compiled at: `%s`\n- Eligibility contract: `%s`\n\n## Corpus\n\n- Fact: %d eligible files, %d notes, %d attachments.\n\n## Links\n\n- Fact: %d local link occurrences.\n- Fact: %d broken or ambiguous local links.\n\n## Orphans\n\n- Schema v1 classification: %d hard orphans.\n- Schema v1 classification: %d notes with no incoming note neighbor.\n- Schema v1 classification: %d outbound-only notes.\n\n## Tags\n\n- Fact: %d normalized tags.\n\n## Frontmatter\n\n- Fact: %d notes have frontmatter.\n\n## Compiler Warnings\n\n- Fact: %d warnings.\n", input.GenerationID, input.CompiledAt, contractHash, c.EligibleFiles, c.Notes, c.Attachments, c.Links, c.BrokenLinks, c.HardOrphans, c.NoBacklinkNotes, c.OutboundOnlyNotes, c.Tags, countFrontmatter(model), c.Warnings)
}

func countFrontmatter(model *graphModel) int {
	count := 0
	for _, note := range model.Notes {
		if note.Syntax.FrontmatterPresent {
			count++
		}
	}
	return count
}

func renderOrphans(input Input, model *graphModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orphans\n\nGeneration: `%s`\n\n", input.GenerationID)
	hard := map[string]int{}
	backlink := map[string]int{}
	outbound := map[string]int{}
	for _, orphan := range model.Orphans {
		if orphan.HardOrphan {
			hard[topLevelFolder(orphan.Path)]++
		}
		if orphan.NoBacklink {
			backlink[topLevelFolder(orphan.Path)]++
		}
		if orphan.OutboundOnly {
			outbound[topLevelFolder(orphan.Path)]++
		}
	}
	fmt.Fprintf(&b, "Summary: %d hard orphans, %d notes without backlinks, %d outbound-only notes.\n\n", countRecords(hard), countRecords(backlink), countRecords(outbound))
	renderFolderCounts(&b, "Hard Orphans By Top-Level Folder", hard)
	renderFolderCounts(&b, "No-Backlink Notes By Top-Level Folder", backlink)
	renderFolderCounts(&b, "Outbound-Only Notes By Top-Level Folder", outbound)
	return b.String()
}

func renderBroken(input Input, model *graphModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Broken Links\n\nGeneration: `%s`\n\n", input.GenerationID)
	type brokenGroup struct {
		status string
		target string
		folder string
	}
	groups := map[brokenGroup]int{}
	for _, broken := range model.Broken {
		groups[brokenGroup{status: broken.ResolutionStatus, target: broken.RawTarget, folder: topLevelFolder(broken.SourcePath)}]++
	}
	keys := make([]brokenGroup, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].status != keys[j].status {
			return keys[i].status < keys[j].status
		}
		if keys[i].target != keys[j].target {
			return keys[i].target < keys[j].target
		}
		return keys[i].folder < keys[j].folder
	})
	for _, key := range keys {
		fmt.Fprintf(&b, "- status=%q source_folder=%q target=%q count=%d\n", key.status, key.folder, key.target, groups[key])
	}
	return b.String()
}

func topLevelFolder(value string) string {
	if idx := strings.IndexByte(value, '/'); idx >= 0 {
		return value[:idx]
	}
	return "(root)"
}

func countRecords(groups map[string]int) int {
	total := 0
	for _, count := range groups {
		total += count
	}
	return total
}

func renderFolderCounts(b *strings.Builder, title string, groups map[string]int) {
	fmt.Fprintf(b, "## %s\n\n", title)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "- %q: %d\n", key, groups[key])
	}
	b.WriteString("\n")
}
