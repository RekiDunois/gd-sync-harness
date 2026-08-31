package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/source"
)

// Input is the complete local compiler input. Policy must be non-nil: a
// missing committed policy is not equivalent to a valid empty policy.
type Input struct {
	ProfileID     string
	ProfileUUID   string
	SourceRoot    string
	MaxFileSize   int64
	Policy        *policy.Snapshot
	PolicyHash    string
	CompilerRunID string
	GenerationID  string
	CompiledAt    string
}

// BuildResult contains a fully materialized generation, excluding MANIFEST.json
// which is created last by the local generation store.
type BuildResult struct {
	Manifest  Manifest
	Artifacts map[string][]byte
}

type noteModel struct {
	Path              string
	Record            FileRecord
	Syntax            *Syntax
	incomingNeighbors map[string]bool
	outgoingNeighbors map[string]bool
}

type graphModel struct {
	Files       []FileRecord
	Notes       map[string]*noteModel
	Links       []LinkRecord
	Broken      []BrokenLinkRecord
	Orphans     []OrphanRecord
	Tags        map[string]*tagModel
	Frontmatter map[string]*FrontmatterField
	Warnings    []WarningRecord
}

type tagModel struct {
	Variants map[string]bool
	Paths    map[string]bool
	Notes    map[string]bool
	Count    int
}

// Compile performs a full scan and retries once if the source changes before
// publication. It does not touch the network or any remote executable.
func Compile(input Input) (*BuildResult, error) {
	if input.Policy == nil {
		return nil, fmt.Errorf("committed policy is required")
	}
	if input.ProfileID == "" || input.ProfileUUID == "" {
		return nil, fmt.Errorf("profile id and profile uuid are required")
	}
	if input.CompilerRunID == "" {
		input.CompilerRunID = uuid.NewString()
	}
	if input.GenerationID == "" {
		input.GenerationID = uuid.NewString()
	}
	if input.CompiledAt == "" {
		input.CompiledAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if input.PolicyHash == "" {
		input.PolicyHash = input.Policy.Hash()
	}
	contractHash := source.EligibilityContractHash(input.PolicyHash, input.MaxFileSize)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		result, snapshot, err := buildOnce(input, contractHash)
		if err != nil {
			return nil, err
		}
		stable, err := validateSnapshot(input, snapshot, input.PolicyHash, contractHash)
		if err != nil {
			return nil, err
		}
		if stable {
			return result, nil
		}
		lastErr = fmt.Errorf("source changed during compile")
	}
	return nil, fmt.Errorf("source_unstable: %w", lastErr)
}

type sourceSnapshot struct {
	Entries []source.Entry
	Hashes  map[string]string
	ID      string
}

func buildOnce(input Input, contractHash string) (*BuildResult, *sourceSnapshot, error) {
	scan, err := source.Scan(source.Options{SourceRoot: input.SourceRoot, MaxFileSize: input.MaxFileSize, Policy: input.Policy})
	if err != nil {
		return nil, nil, err
	}
	model := &graphModel{Notes: map[string]*noteModel{}, Tags: map[string]*tagModel{}, Frontmatter: map[string]*FrontmatterField{}}
	hashes := make(map[string]string)
	parser := NewParser()
	for _, entry := range scan.Entries {
		record := FileRecord{
			SchemaVersion: SchemaVersion, CompileGenerationID: input.GenerationID,
			Path: entry.RelPath, PathID: digestPath(entry.RelPath), Size: entry.Size,
			MTimeUnixNano: entry.ModTimeNS, Kind: "attachment", Extension: path.Ext(entry.RelPath),
		}
		if strings.EqualFold(path.Ext(entry.RelPath), ".md") {
			record.Kind = "note"
			content, err := readSourceFile(input.SourceRoot, entry.RelPath)
			if err != nil {
				return nil, nil, err
			}
			hash := sha256.Sum256(content)
			hexHash := hex.EncodeToString(hash[:])
			record.ContentSHA256 = &hexHash
			hashes[entry.RelPath] = hexHash
			syntax, err := parser.Parse(content)
			if err != nil {
				return nil, nil, fmt.Errorf("parse %q: %w", entry.RelPath, err)
			}
			record.Frontmatter = frontmatterSummary(syntax)
			for field, value := range syntax.Frontmatter {
				addFrontmatterField(model.Frontmatter, field, value)
			}
			for _, warning := range syntax.Warnings {
				model.Warnings = append(model.Warnings, WarningRecord{
					SchemaVersion: SchemaVersion, CompileGenerationID: input.GenerationID,
					Code: warning.Code, Path: entry.RelPath, Message: warning.Message,
				})
			}
			addFrontmatterTags(model, entry.RelPath, syntax)
			model.Notes[entry.RelPath] = &noteModel{Path: entry.RelPath, Record: record, Syntax: syntax,
				incomingNeighbors: map[string]bool{}, outgoingNeighbors: map[string]bool{}}
		}
		model.Files = append(model.Files, record)
	}
	sort.Slice(model.Files, func(i, j int) bool { return model.Files[i].Path < model.Files[j].Path })
	resolveGraph(model)
	result, err := serialize(input, contractHash, model)
	if err != nil {
		return nil, nil, err
	}
	snapshot := &sourceSnapshot{Entries: append([]source.Entry(nil), scan.Entries...), Hashes: hashes}
	snapshot.ID = snapshotDigest(snapshot.Entries, snapshot.Hashes, input.PolicyHash, contractHash)
	result.Manifest.SourceSnapshotID = snapshot.ID
	result.Manifest.Counts = counts(model)
	// The manifest inventory is computed over all detail artifacts and excludes
	// MANIFEST.json by construction.
	result.Manifest.Artifacts = artifactInventory(result.Artifacts)
	return result, snapshot, nil
}

func readSourceFile(root, rel string) ([]byte, error) {
	return osReadFile(filepathJoin(root, rel))
}

// These small indirections keep the compiler's source operations easy to fault
// inject in publication tests without storing source bytes in graph state.
var osReadFile = func(name string) ([]byte, error) { return os.ReadFile(name) }
var filepathJoin = filepath.Join

func validateSnapshot(input Input, before *sourceSnapshot, policyHash, contractHash string) (bool, error) {
	scan, err := source.Scan(source.Options{SourceRoot: input.SourceRoot, MaxFileSize: input.MaxFileSize, Policy: input.Policy})
	if err != nil {
		return false, err
	}
	if len(scan.Entries) != len(before.Entries) {
		return false, nil
	}
	for i, entry := range scan.Entries {
		old := before.Entries[i]
		if entry != old {
			return false, nil
		}
		if strings.EqualFold(path.Ext(entry.RelPath), ".md") {
			content, err := readSourceFile(input.SourceRoot, entry.RelPath)
			if err != nil {
				return false, err
			}
			hash := sha256.Sum256(content)
			if hex.EncodeToString(hash[:]) != before.Hashes[entry.RelPath] {
				return false, nil
			}
		}
	}
	if input.Policy.Hash() != policyHash || source.EligibilityContractHash(policyHash, input.MaxFileSize) != contractHash {
		return false, nil
	}
	return true, nil
}

func snapshotDigest(entries []source.Entry, hashes map[string]string, policyHash, contractHash string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00", source.SnapshotEncodingVersion, policyHash, contractHash)
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00%s\x00", entry.RelPath, entry.Size, entry.ModTimeNS, hashes[entry.RelPath])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func digestPath(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func frontmatterSummary(syntax *Syntax) *FrontmatterNoteSummary {
	if !syntax.FrontmatterPresent {
		return nil
	}
	summary := &FrontmatterNoteSummary{Present: true, Fields: map[string]string{}, Types: map[string]string{}}
	keys := make([]string, 0, len(syntax.Frontmatter))
	for key := range syntax.Frontmatter {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		summary.Fields[key] = "present"
		summary.Types[key] = valueType(syntax.Frontmatter[key])
	}
	return summary
}

func addFrontmatterField(fields map[string]*FrontmatterField, name string, value any) {
	field := fields[name]
	if field == nil {
		field = &FrontmatterField{TypeCounts: map[string]int{}}
		fields[name] = field
	}
	field.PresentCount++
	typ := valueType(value)
	field.TypeCounts[typ]++
	if value == nil {
		field.NullCount++
	}
}

func valueType(value any) string {
	if value == nil {
		return "null"
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "sequence"
	case reflect.Map, reflect.Struct:
		return "mapping"
	default:
		return "other"
	}
}

func addFrontmatterTags(model *graphModel, notePath string, syntax *Syntax) {
	if raw, ok := syntax.Frontmatter["tags"]; ok {
		switch value := raw.(type) {
		case string:
			addTag(model, NormalizeTag(value), value, notePath)
		case []any:
			for _, item := range value {
				if tag, ok := item.(string); ok {
					addTag(model, NormalizeTag(tag), tag, notePath)
				} else {
					model.Warnings = append(model.Warnings, WarningRecord{SchemaVersion: SchemaVersion, Code: "frontmatter_tag_type_unsupported", Path: notePath, Message: "tags sequence contains a non-scalar value"})
				}
			}
		default:
			if raw != nil {
				model.Warnings = append(model.Warnings, WarningRecord{SchemaVersion: SchemaVersion, Code: "frontmatter_tag_type_unsupported", Path: notePath, Message: "tags must be a scalar or sequence of scalars"})
			}
		}
	}
	if raw, ok := syntax.Frontmatter["aliases"]; ok {
		switch value := raw.(type) {
		case nil, string:
		case []any:
			for _, item := range value {
				if _, ok := item.(string); !ok && item != nil {
					model.Warnings = append(model.Warnings, WarningRecord{SchemaVersion: SchemaVersion, Code: "frontmatter_alias_type_unsupported", Path: notePath, Message: "aliases sequence contains a non-scalar value"})
					break
				}
			}
		default:
			model.Warnings = append(model.Warnings, WarningRecord{SchemaVersion: SchemaVersion, Code: "frontmatter_alias_type_unsupported", Path: notePath, Message: "aliases must be a scalar or sequence"})
		}
	}
	for _, tag := range syntax.Tags {
		addTag(model, NormalizeTag(tag.Tag), "#"+tag.Tag, notePath)
	}
}

func addTag(model *graphModel, normalized, variant, notePath string) {
	if normalized == "" {
		return
	}
	tag := model.Tags[normalized]
	if tag == nil {
		tag = &tagModel{Variants: map[string]bool{}, Paths: map[string]bool{}, Notes: map[string]bool{}}
		model.Tags[normalized] = tag
	}
	tag.Count++
	tag.Variants[variant] = true
	tag.Paths[notePath] = true
	tag.Notes[notePath] = true
}

func resolveGraph(model *graphModel) {
	allPaths := map[string]bool{}
	notePaths := map[string]bool{}
	for file := range model.Notes {
		allPaths[file] = true
		notePaths[file] = true
	}
	for _, file := range model.Files {
		if file.Kind == "attachment" {
			allPaths[file.Path] = true
		}
	}
	noteNames := make([]string, 0, len(model.Notes))
	for name := range model.Notes {
		noteNames = append(noteNames, name)
	}
	sort.Strings(noteNames)
	for _, noteName := range noteNames {
		note := model.Notes[noteName]
		for _, fact := range note.Syntax.Links {
			if IsExternalTarget(fact.RawTarget) {
				continue
			}
			status, target, candidates, fragment := resolveTarget(note.Path, fact, allPaths, notePaths)
			link := LinkRecord{SchemaVersion: SchemaVersion, SourcePath: note.Path, LinkKind: fact.Kind, RawTarget: fact.RawTarget, DisplayText: fact.Display, Fragment: fragment, ResolutionStatus: status, CandidatePaths: candidates}
			if target != "" {
				link.ResolvedTargetPath = &target
				if notePaths[target] {
					link.TargetKind = "note"
				} else {
					link.TargetKind = "attachment"
				}
			}
			model.Links = append(model.Links, link)
		}
	}
	for i := range model.Links {
		link := &model.Links[i]
		sourceNote := model.Notes[link.SourcePath]
		sourceNote.Record.OutgoingEdgeCount++
		switch link.ResolutionStatus {
		case "unresolved":
			sourceNote.Record.UnresolvedCount++
		case "ambiguous":
			sourceNote.Record.AmbiguousCount++
		case "resolved":
			target := model.Notes[valueOrEmpty(link.ResolvedTargetPath)]
			if target != nil {
				sourceNote.Record.IncomingEdgeCount += 0
				target.Record.IncomingEdgeCount++
				if target.Path == sourceNote.Path {
					sourceNote.Record.SelfLinkCount++
				} else {
					addUniqueNeighbor(sourceNote, true, target.Path)
					addUniqueNeighbor(target, false, sourceNote.Path)
				}
			}
		}
	}
	for _, noteName := range noteNames {
		note := model.Notes[noteName]
		note.Record.Tags = noteTags(model, note.Path)
		model.Files = replaceFileRecord(model.Files, note.Record)
		if note.Record.IncomingNoteCount == 0 {
			orphan := OrphanRecord{SchemaVersion: SchemaVersion, Path: note.Path, NoBacklink: true, IncomingNoteCount: note.Record.IncomingNoteCount, OutgoingNoteCount: note.Record.OutgoingNoteCount, IncomingEdgeCount: note.Record.IncomingEdgeCount, OutgoingEdgeCount: note.Record.OutgoingEdgeCount, UnresolvedCount: note.Record.UnresolvedCount, AmbiguousCount: note.Record.AmbiguousCount, SelfLinkCount: note.Record.SelfLinkCount}
			orphan.HardOrphan = orphan.OutgoingNoteCount == 0
			orphan.OutboundOnly = orphan.OutgoingNoteCount > 0
			model.Orphans = append(model.Orphans, orphan)
		}
	}
	for _, link := range model.Links {
		if link.ResolutionStatus != "resolved" {
			model.Broken = append(model.Broken, BrokenLinkRecord{SchemaVersion: SchemaVersion, SourcePath: link.SourcePath, LinkKind: link.LinkKind, RawTarget: link.RawTarget, Fragment: link.Fragment, ResolutionStatus: link.ResolutionStatus, CandidatePaths: link.CandidatePaths})
		}
	}
	sort.Slice(model.Orphans, func(i, j int) bool { return model.Orphans[i].Path < model.Orphans[j].Path })
	sort.Slice(model.Broken, func(i, j int) bool {
		if model.Broken[i].ResolutionStatus != model.Broken[j].ResolutionStatus {
			return model.Broken[i].ResolutionStatus < model.Broken[j].ResolutionStatus
		}
		if model.Broken[i].RawTarget != model.Broken[j].RawTarget {
			return model.Broken[i].RawTarget < model.Broken[j].RawTarget
		}
		return model.Broken[i].SourcePath < model.Broken[j].SourcePath
	})
}

func resolveTarget(sourcePath string, fact LinkFact, allPaths, notePaths map[string]bool) (string, string, []string, string) {
	raw := fact.RawTarget
	fragment := fact.Fragment
	if fact.Kind != "wikilink" {
		if idx := strings.Index(raw, "#"); idx >= 0 {
			fragment = raw[idx+1:]
			raw = raw[:idx]
		}
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || decoded == "" {
		return "unresolved", "", nil, fragment
	}
	var candidates []string
	if fact.Kind == "wikilink" {
		if strings.HasPrefix(decoded, "./") {
			decoded = strings.TrimPrefix(decoded, "./")
		}
		if !strings.Contains(decoded, "/") && path.Ext(decoded) == "" {
			for candidate := range notePaths {
				if strings.TrimSuffix(path.Base(candidate), path.Ext(candidate)) == decoded {
					candidates = append(candidates, candidate)
				}
			}
		} else {
			if allPaths[decoded] {
				candidates = append(candidates, decoded)
			}
			if path.Ext(decoded) == "" && notePaths[decoded+".md"] {
				candidates = append(candidates, decoded+".md")
			}
		}
	} else {
		if strings.HasPrefix(decoded, "/") {
			return "unresolved", "", nil, fragment
		}
		canonical := path.Join(path.Dir(sourcePath), decoded)
		if canonical == "." || strings.HasPrefix(canonical, "../") || canonical == ".." {
			return "unresolved", "", nil, fragment
		}
		if allPaths[canonical] {
			candidates = append(candidates, canonical)
		}
		if path.Ext(canonical) == "" && notePaths[canonical+".md"] {
			candidates = append(candidates, canonical+".md")
		}
	}
	sort.Strings(candidates)
	candidates = dedupStrings(candidates)
	if len(candidates) == 1 {
		return "resolved", candidates[0], nil, fragment
	}
	if len(candidates) > 1 {
		return "ambiguous", "", candidates, fragment
	}
	return "unresolved", "", nil, fragment
}

func addUniqueNeighbor(note *noteModel, incoming bool, path string) {
	set := note.incomingNeighbors
	if !incoming {
		set = note.outgoingNeighbors
	}
	set[path] = true
	if incoming {
		note.Record.IncomingNoteCount = len(set)
	} else {
		note.Record.OutgoingNoteCount = len(set)
	}
}

func noteTags(model *graphModel, notePath string) []string {
	var tags []string
	for tag, value := range model.Tags {
		if value.Paths[notePath] {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags
}

func replaceFileRecord(files []FileRecord, record FileRecord) []FileRecord {
	for i := range files {
		if files[i].Path == record.Path {
			files[i] = record
			break
		}
	}
	return files
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func dedupStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
