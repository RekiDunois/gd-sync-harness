package compiler

const (
	SchemaVersion   = 1
	CompilerVersion = "knowledge-compiler-v1"
)

type FrontmatterField struct {
	PresentCount int            `json:"present_count"`
	NullCount    int            `json:"null_count"`
	TypeCounts   map[string]int `json:"type_counts"`
}

type FrontmatterNoteSummary struct {
	Present bool              `json:"present"`
	Fields  map[string]string `json:"fields,omitempty"`
	Types   map[string]string `json:"types,omitempty"`
}

type FileRecord struct {
	SchemaVersion       int                     `json:"schema_version"`
	CompileGenerationID string                  `json:"compile_generation_id"`
	Path                string                  `json:"path"`
	PathID              string                  `json:"path_id"`
	Kind                string                  `json:"kind"`
	Extension           string                  `json:"extension"`
	Size                int64                   `json:"size"`
	MTimeUnixNano       int64                   `json:"mtime_unix_nano"`
	ContentSHA256       *string                 `json:"content_sha256,omitempty"`
	Frontmatter         *FrontmatterNoteSummary `json:"frontmatter,omitempty"`
	Tags                []string                `json:"tags,omitempty"`
	IncomingEdgeCount   int                     `json:"incoming_edge_count,omitempty"`
	OutgoingEdgeCount   int                     `json:"outgoing_edge_count,omitempty"`
	IncomingNoteCount   int                     `json:"incoming_note_count,omitempty"`
	OutgoingNoteCount   int                     `json:"outgoing_note_count,omitempty"`
	SelfLinkCount       int                     `json:"self_link_count,omitempty"`
	UnresolvedCount     int                     `json:"unresolved_link_count,omitempty"`
	AmbiguousCount      int                     `json:"ambiguous_link_count,omitempty"`
}

type LinkRecord struct {
	SchemaVersion       int      `json:"schema_version"`
	CompileGenerationID string   `json:"compile_generation_id"`
	SourcePath          string   `json:"source_path"`
	LinkKind            string   `json:"link_kind"`
	RawTarget           string   `json:"raw_target"`
	DisplayText         string   `json:"display_text,omitempty"`
	Fragment            string   `json:"fragment,omitempty"`
	ResolutionStatus    string   `json:"resolution_status"`
	ResolvedTargetPath  *string  `json:"resolved_target_path,omitempty"`
	TargetKind          string   `json:"target_kind,omitempty"`
	CandidatePaths      []string `json:"candidate_paths,omitempty"`
}

type OrphanRecord struct {
	SchemaVersion       int    `json:"schema_version"`
	CompileGenerationID string `json:"compile_generation_id"`
	Path                string `json:"path"`
	HardOrphan          bool   `json:"hard_orphan"`
	NoBacklink          bool   `json:"no_backlink"`
	OutboundOnly        bool   `json:"outbound_only"`
	IncomingNoteCount   int    `json:"incoming_note_count"`
	OutgoingNoteCount   int    `json:"outgoing_note_count"`
	IncomingEdgeCount   int    `json:"incoming_edge_count"`
	OutgoingEdgeCount   int    `json:"outgoing_edge_count"`
	UnresolvedCount     int    `json:"unresolved_link_count"`
	AmbiguousCount      int    `json:"ambiguous_link_count"`
	SelfLinkCount       int    `json:"self_link_count"`
}

type BrokenLinkRecord struct {
	SchemaVersion       int      `json:"schema_version"`
	CompileGenerationID string   `json:"compile_generation_id"`
	SourcePath          string   `json:"source_path"`
	LinkKind            string   `json:"link_kind"`
	RawTarget           string   `json:"raw_target"`
	Fragment            string   `json:"fragment,omitempty"`
	ResolutionStatus    string   `json:"resolution_status"`
	CandidatePaths      []string `json:"candidate_paths,omitempty"`
}

type TagRecord struct {
	SchemaVersion       int      `json:"schema_version"`
	CompileGenerationID string   `json:"compile_generation_id"`
	Tag                 string   `json:"tag"`
	NoteCount           int      `json:"note_count"`
	OccurrenceCount     int      `json:"occurrence_count"`
	Variants            []string `json:"variants"`
	NotePaths           []string `json:"note_paths"`
}

type WarningRecord struct {
	SchemaVersion       int    `json:"schema_version"`
	CompileGenerationID string `json:"compile_generation_id"`
	Code                string `json:"code"`
	Path                string `json:"path,omitempty"`
	Message             string `json:"message"`
}

type AggregateCounts struct {
	EligibleFiles     int `json:"eligible_files"`
	Notes             int `json:"notes"`
	Attachments       int `json:"attachments"`
	Links             int `json:"link_occurrences"`
	BrokenLinks       int `json:"broken_links"`
	HardOrphans       int `json:"hard_orphans"`
	NoBacklinkNotes   int `json:"no_backlink_notes"`
	OutboundOnlyNotes int `json:"outbound_only_notes"`
	Tags              int `json:"tags"`
	Warnings          int `json:"warnings"`
}

type ArtifactInfo struct {
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	Records int    `json:"records,omitempty"`
}

type Manifest struct {
	SchemaVersion           int                     `json:"schema_version"`
	CompilerVersion         string                  `json:"compiler_version"`
	ProfileID               string                  `json:"profile_id"`
	ProfileUUID             string                  `json:"profile_uuid"`
	CompilerRunID           string                  `json:"compiler_run_id"`
	CompileGenerationID     string                  `json:"compile_generation_id"`
	CompiledAt              string                  `json:"compiled_at"`
	SourceSnapshotID        string                  `json:"source_snapshot_id"`
	PolicyHash              string                  `json:"policy_hash"`
	EligibilityContractHash string                  `json:"eligibility_contract_hash"`
	Counts                  AggregateCounts         `json:"counts"`
	Artifacts               map[string]ArtifactInfo `json:"artifacts"`
}

type RootPointer struct {
	SchemaVersion            int    `json:"schema_version"`
	ProfileID                string `json:"profile_id"`
	ProfileUUID              string `json:"profile_uuid"`
	CurrentGenerationID      string `json:"current_generation_id"`
	GenerationManifestPath   string `json:"generation_manifest_path"`
	GenerationManifestBytes  int    `json:"generation_manifest_bytes"`
	GenerationManifestSHA256 string `json:"generation_manifest_sha256"`
	PublishedAt              string `json:"published_at"`
}
