package engine

// View types match the management page JSON used by the previous prompt-admin panel.

type Candidate struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Score   *int   `json:"score"`
	Reason  string `json:"reason"`
	Preview string `json:"preview,omitempty"`
}

type Row struct {
	Model      string      `json:"model"`
	Upstream   string      `json:"upstream,omitempty"`
	InCPA      bool        `json:"in_cpa"`
	Candidates []Candidate `json:"candidates"`
	Provider   string      `json:"provider,omitempty"`
	Pointer    bool        `json:"pointer"`
	OldNames   []string    `json:"old_names"`
	Source     string      `json:"source"`
	Key        string      `json:"key"`
	Score      *int        `json:"score"`
	Reason     string      `json:"reason"`
	Prompt     string      `json:"prompt"`
	Error      string      `json:"error"`
}

type Conflict struct {
	Model      string      `json:"model"`
	Upstream   string      `json:"upstream,omitempty"`
	Why        string      `json:"why"`
	Previous   string      `json:"previous"`
	Candidates []Candidate `json:"candidates"`
}

type RepoStatus struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Commit   string `json:"commit"`
	Files    int    `json:"files"`
	PulledAt string `json:"pulled_at"`
}

type FileRef struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type Status struct {
	LastRun       string                `json:"last_run,omitempty"`
	LastRunKind   string                `json:"last_run_kind,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	LastSync      string                `json:"last_sync,omitempty"`
	DurationS     float64               `json:"duration_s,omitempty"`
	Changed       bool                  `json:"changed"`
	ModelSource   string                `json:"model_source,omitempty"`
	Models        []string              `json:"models,omitempty"`
	Repos         map[string]RepoStatus `json:"repos,omitempty"`
	Sig           signature             `json:"sig"`
	HiddenAliases map[string]string     `json:"hidden_aliases,omitempty"`
	StoredOnly    []string              `json:"stored_only,omitempty"`
	Rows          []Row                 `json:"rows,omitempty"`
	Conflicts     []Conflict            `json:"conflicts,omitempty"`
	Unmatched     []string              `json:"unmatched,omitempty"`
	Unused        []FileRef             `json:"unused,omitempty"`
	UnusedTotal   int                   `json:"unused_total"`
	Files         []FileRef             `json:"files,omitempty"`
	FilesTotal    int                   `json:"files_total"`
	OutputKeys    []string              `json:"output_keys,omitempty"`
	Error         string                `json:"error,omitempty"`
}

type RepoView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Subpath  string `json:"subpath"`
	Enabled  bool   `json:"enabled"`
	AddedAt  string `json:"added_at"`
	HasToken bool   `json:"has_token"`
}

type SyncInfo struct {
	Running bool   `json:"running"`
	Started string `json:"started"`
	Kind    string `json:"kind"`
}

type Snapshot struct {
	Repos         []RepoView          `json:"repos"`
	Overrides     map[string]Override `json:"overrides"`
	Local         map[string]string   `json:"local"`
	Choices       map[string]Choice   `json:"choices"`
	DefaultPrompt string              `json:"default_prompt"`
	Status        Status              `json:"status"`
	Log           []string            `json:"log"`
	Sync          SyncInfo            `json:"sync"`
}
