package engine

import (
	"context"
	"sort"
	"strings"
	"time"

	"cpa-plugin-prompt-admin/internal/match"
)

const localKey = "local:"

type indexedFile struct {
	key  string
	repo Repo
	path string
	fp   match.FileProfile
}

func (e *Engine) computeBody(pull bool, reason string) error {
	snap := e.fetchModels()
	names := loadNames(e.cfg.NamesFile)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.computeLocked(pull, reason, snap, names)
}

func (e *Engine) computeLocked(pull bool, reason string, snap modelSnap, names namesDoc) error {
	started := time.Now()
	if len(snap.IDs) == 0 {
		e.status.Error = "no models from CPA, config.yaml, or the static models list; prompts.json was left unchanged"
		e.status.LastRun = e.stamp()
		e.status.Reason = reason
		_ = e.saveStatusLocked()
		e.logf("ERROR apply refused: %s", e.status.Error)
		return UserError(e.status.Error)
	}
	st := e.state.clone()
	prevStatus := e.status
	repoStatus := map[string]RepoStatus{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, repo := range st.Repos {
		rs := RepoStatus{ID: repo.ID, OK: true}
		if repo.IsEnabled() && pull {
			commit, err := e.pullRepo(ctx, repo)
			if err != nil {
				rs.OK = false
				rs.Error = trimRunes(err.Error(), 300)
				e.logf("pull FAILED %s: %s", fileLabel(repo, ""), rs.Error)
			} else {
				rs.Commit = commit
				rs.PulledAt = e.stamp()
				e.logf("pull ok %s commit=%s", fileLabel(repo, ""), commit)
			}
		}
		repoStatus[repo.ID] = rs
	}
	for id, rs := range repoStatus {
		old := prevStatus.Repos[id]
		if !pull || !rs.OK {
			if rs.Commit == "" {
				rs.Commit = old.Commit
			}
			if rs.PulledAt == "" {
				rs.PulledAt = old.PulledAt
			}
			if !pull && old.Error != "" {
				rs.OK = old.OK
				rs.Error = old.Error
			}
			repoStatus[id] = rs
		}
	}

	reposByKey := map[string]Repo{}
	var files []indexedFile
	for _, repo := range st.Repos {
		if !repo.IsEnabled() {
			continue
		}
		reposByKey[repoKey(repo)] = repo
		scanned := e.scanRepo(repo)
		rs := repoStatus[repo.ID]
		rs.Files = len(scanned)
		repoStatus[repo.ID] = rs
		for _, file := range scanned {
			files = append(files, indexedFile{
				key:  repoKey(repo) + "::" + file.Path,
				repo: repo,
				path: file.Path,
				fp:   match.FileProfileOf(file.Path),
			})
		}
	}

	currentFile := readPromptFile(e.promptsPath())
	current := currentFile.Prompts
	if !st.HasLast {
		for key, value := range current {
			if _, exists := st.Local[key]; !exists {
				st.Local[key] = value
			}
		}
		if len(current) > 0 {
			keys := append([]string(nil), currentFile.Order...)
			e.logf("imported %d existing prompts as 本地提示词: %s", len(current), strings.Join(keys, ", "))
		}
	} else {
		for key, value := range current {
			last, had := st.LastOutput[key]
			local, hasLocal := st.Local[key]
			if (!had || last != value) && (!hasLocal || local != value) {
				st.Local[key] = value
				e.logf("adopted external change for %s into 本地提示词", key)
			}
		}
		for key, value := range st.LastOutput {
			if _, still := current[key]; !still && st.Local[key] == value {
				delete(st.Local, key)
				e.logf("%s was removed externally; dropped from 本地提示词", key)
			}
		}
	}

	modelSet := map[string]struct{}{}
	for _, id := range snap.IDs {
		modelSet[id] = struct{}{}
	}
	oldNames := map[string][]string{}
	for old, next := range names.Aliases {
		oldNames[next] = append(oldNames[next], old)
		if _, live := modelSet[next]; !live {
			continue
		}
		if _, oldLive := modelSet[old]; oldLive {
			continue
		}
		moved := []string{}
		if _, exists := st.Local[next]; !exists {
			if value, ok := st.Local[old]; ok {
				st.Local[next] = value
				delete(st.Local, old)
				moved = append(moved, "local")
			}
		}
		if _, exists := st.Overrides[next]; !exists {
			if value, ok := st.Overrides[old]; ok {
				st.Overrides[next] = value
				delete(st.Overrides, old)
				moved = append(moved, "overrides")
			}
		}
		if _, exists := st.Choices[next]; !exists {
			if value, ok := st.Choices[old]; ok {
				st.Choices[next] = value
				delete(st.Choices, old)
				moved = append(moved, "choices")
			}
		}
		_, inLocal := st.Local[next]
		_, inOver := st.Overrides[next]
		_, inChoice := st.Choices[next]
		localMoved := false
		for _, item := range moved {
			if item == "local" {
				localMoved = true
			}
		}
		if value, ok := current[old]; ok && !inLocal && !inOver && !inChoice && !localMoved {
			st.Local[next] = value
			moved = append(moved, "prompt")
		}
		if len(moved) > 0 {
			e.logf("renamed model %s -> %s: moved %s", old, next, strings.Join(moved, ", "))
		}
	}
	for key := range oldNames {
		sort.Strings(oldNames[key])
	}

	cache := map[string]match.Profile{}
	output := map[string]string{}
	var rows []Row
	var conflicts []Conflict
	var unmatched []string
	used := map[string]struct{}{}
	fallback := strings.TrimSpace(st.DefaultPrompt)
	for _, model := range snap.IDs {
		upstream := snap.Upstream[model]
		var scored []indexedHit
		best := 0
		for _, file := range files {
			score, why := match.ScoreModel(model, upstream, file.fp, cache)
			if score >= match.MinScore {
				scored = append(scored, indexedHit{score: score, why: why, file: file})
				if score > best {
					best = score
				}
			}
		}
		var repoCands []Candidate
		for _, hit := range scored {
			if hit.score != best {
				continue
			}
			repoCands = append(repoCands, Candidate{
				Key:    hit.file.key,
				Label:  fileLabel(hit.file.repo, hit.file.path),
				Score:  intPtr(hit.score),
				Reason: hit.why,
			})
		}
		sort.Slice(repoCands, func(i, j int) bool { return repoCands[i].Label < repoCands[j].Label })
		localText, hasLocal := st.Local[model]
		if hasLocal {
			for _, cand := range repoCands {
				if text, ok := e.readRepoFile(reposByKey, cand.Key); ok && strings.TrimSpace(text) == strings.TrimSpace(localText) {
					hasLocal = false
					break
				}
			}
		}
		cands := append([]Candidate(nil), repoCands...)
		if hasLocal {
			cands = append([]Candidate{{
				Key:    localKey,
				Label:  "本地提示词（面板/现有）",
				Reason: "现有/手动保存的提示词",
			}}, cands...)
		}
		for _, cand := range repoCands {
			used[cand.Key] = struct{}{}
		}
		row := Row{
			Model: model, Upstream: upstream, InCPA: true, Candidates: cands,
			Provider: names.Provider[model], Pointer: names.Pointer[model],
			OldNames: nonNil(oldNames[model]), Source: "none",
		}
		textOf := func(key string) (string, bool) {
			if key == localKey {
				value, ok := st.Local[model]
				return value, ok
			}
			return e.readRepoFile(reposByKey, key)
		}
		var prompt *string
		setPrompt := func(value string) { prompt = &value }
		if ov, ok := st.Overrides[model]; ok {
			switch ov.Type {
			case "text":
				setPrompt(ov.Text)
				row.Source = "override-text"
			case "none":
				setPrompt("")
				row.Source = "override-none"
			case "file":
				row.Source = "override-file"
				row.Key = ov.Key
				used[ov.Key] = struct{}{}
				if text, ok := textOf(ov.Key); ok {
					setPrompt(text)
				} else {
					row.Error = "覆盖指定的文件已不存在，暂时保留上一次的提示词"
					setPrompt(current[model])
				}
			}
		}
		if prompt == nil {
			switch {
			case len(cands) == 0:
				if fallback != "" {
					setPrompt(fallback)
					row.Source = "default"
				} else {
					setPrompt("")
				}
			case len(cands) == 1:
				cand := cands[0]
				if text, ok := textOf(cand.Key); ok {
					setPrompt(text)
					row.Source = "repo"
					if cand.Key == localKey {
						row.Source = "local"
					}
					row.Key = cand.Key
					row.Score = cand.Score
					row.Reason = cand.Reason
				}
			default:
				choice := st.Choices[model]
				keys := candidateKeys(cands)
				if choice.Key != "" && contains(keys, choice.Key) && subset(keys, choice.Seen) {
					cand := candByKey(cands, choice.Key)
					if text, ok := textOf(cand.Key); ok {
						setPrompt(text)
						row.Source = "chosen"
						row.Key = cand.Key
						row.Score = cand.Score
						row.Reason = "已选择：" + cand.Reason
					}
				} else {
					why := "首次出现多个候选"
					if choice.Key != "" && !contains(keys, choice.Key) {
						why = "之前选择的文件已消失"
					} else if choice.Key != "" {
						why = "出现了新的候选文件"
					}
					prev := current[model]
					if prev == "" {
						for _, old := range oldNames[model] {
							if current[old] != "" {
								prev = current[old]
								break
							}
						}
					}
					if prev != "" {
						setPrompt(prev)
					} else {
						setPrompt(fallback)
					}
					row.Source = "conflict"
					row.Reason = why
					shown := make([]Candidate, len(cands))
					for i, cand := range cands {
						text, _ := textOf(cand.Key)
						cand.Preview = trimRunes(text, maxPreview)
						shown[i] = cand
					}
					conflicts = append(conflicts, Conflict{
						Model: model, Upstream: upstream, Why: why, Previous: prev, Candidates: shown,
					})
				}
			}
			if prompt == nil {
				row.Error = "读取候选文件失败，保留上一次的提示词"
				setPrompt(current[model])
			}
		}
		if len(repoCands) == 0 {
			unmatched = append(unmatched, model)
		}
		value := ""
		if prompt != nil {
			value = *prompt
		}
		row.Prompt = value
		if value != "" {
			output[model] = value
		}
		rows = append(rows, row)
	}

	var unused []FileRef
	var allFiles []FileRef
	for _, file := range files {
		ref := FileRef{Key: file.key, Label: fileLabel(file.repo, file.path)}
		allFiles = append(allFiles, ref)
		if _, ok := used[file.key]; !ok {
			unused = append(unused, ref)
		}
	}
	order := make([]string, 0, len(output))
	seen := map[string]struct{}{}
	for _, key := range currentFile.Order {
		if _, ok := output[key]; ok {
			order = append(order, key)
			seen[key] = struct{}{}
		}
	}
	for _, model := range snap.IDs {
		if _, ok := output[model]; ok {
			if _, already := seen[model]; !already {
				order = append(order, model)
			}
		}
	}
	changed := !mapsEqual(output, current)
	if changed {
		if err := backupPrompts(e.backupDir(), e.promptsPath(), e.now()); err != nil {
			return err
		}
		if err := writePromptFile(e.promptsPath(), order, output); err != nil {
			return err
		}
		e.book.setPrompts(output)
		var diff []string
		seenDiff := map[string]struct{}{}
		for key := range output {
			if current[key] != output[key] {
				diff = append(diff, key)
				seenDiff[key] = struct{}{}
			}
		}
		for key := range current {
			if _, ok := seenDiff[key]; !ok {
				if _, still := output[key]; !still {
					diff = append(diff, key)
				}
			}
		}
		sort.Strings(diff)
		e.logf("prompts.json updated (%d entries; changed: %s)", len(output), strings.Join(diff, ", "))
	} else {
		e.book.setPrompts(output)
	}
	st.HasLast = true
	st.LastOutput = cloneMap(output)
	if err := saveState(e.statePath(), st); err != nil {
		return err
	}
	e.state = st
	stored := map[string]struct{}{}
	for key := range st.Local {
		stored[key] = struct{}{}
	}
	for key := range st.Overrides {
		stored[key] = struct{}{}
	}
	var storedOnly []string
	for key := range stored {
		if _, live := modelSet[key]; !live {
			storedOnly = append(storedOnly, key)
		}
	}
	sort.Strings(storedOnly)
	kind := "apply"
	if pull {
		kind = "sync"
	}
	status := Status{
		LastRun: e.stamp(), LastRunKind: kind, Reason: reason,
		LastSync: prevStatus.LastSync, DurationS: roundSeconds(time.Since(started)),
		Changed: changed, ModelSource: snap.Source, Models: append([]string(nil), snap.IDs...),
		Repos: repoStatus, Sig: e.signature(snap), HiddenAliases: names.Aliases,
		StoredOnly: storedOnly, Rows: rows, Conflicts: conflicts, Unmatched: unmatched,
		UnusedTotal: len(unused), FilesTotal: len(allFiles), OutputKeys: order,
	}
	if pull {
		status.LastSync = status.LastRun
	}
	if len(unused) > 2000 {
		status.Unused = unused[:2000]
	} else {
		status.Unused = unused
	}
	if len(allFiles) > 5000 {
		status.Files = allFiles[:5000]
	} else {
		status.Files = allFiles
	}
	if status.Unused == nil {
		status.Unused = []FileRef{}
	}
	if status.Files == nil {
		status.Files = []FileRef{}
	}
	if status.Rows == nil {
		status.Rows = []Row{}
	}
	if status.Conflicts == nil {
		status.Conflicts = []Conflict{}
	}
	if status.Unmatched == nil {
		status.Unmatched = []string{}
	}
	e.status = status
	if err := e.saveStatusLocked(); err != nil {
		return err
	}
	e.logf("%s done reason=%s models=%d files=%d conflicts=%d changed=%t in %.2fs",
		kind, reason, len(snap.IDs), len(files), len(conflicts), changed, status.DurationS)
	return nil
}

type indexedHit struct {
	score int
	why   string
	file  indexedFile
}

func intPtr(v int) *int { return &v }

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func candidateKeys(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, cand := range cands {
		out[i] = cand.Key
	}
	return out
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func subset(items, seen []string) bool {
	have := map[string]struct{}{}
	for _, item := range seen {
		have[item] = struct{}{}
	}
	for _, item := range items {
		if _, ok := have[item]; !ok {
			return false
		}
	}
	return true
}

func candByKey(cands []Candidate, key string) Candidate {
	for _, cand := range cands {
		if cand.Key == key {
			return cand
		}
	}
	return Candidate{}
}

func roundSeconds(d time.Duration) float64 {
	return float64(int(d.Seconds()*100+0.5)) / 100
}

func (s State) clone() State {
	out := s
	out.Repos = append([]Repo(nil), s.Repos...)
	out.Overrides = map[string]Override{}
	for k, v := range s.Overrides {
		out.Overrides[k] = v
	}
	out.Local = cloneMap(s.Local)
	out.Choices = map[string]Choice{}
	for k, v := range s.Choices {
		v.Seen = append([]string(nil), v.Seen...)
		out.Choices[k] = v
	}
	if s.LastOutput != nil {
		out.LastOutput = cloneMap(s.LastOutput)
	}
	return out
}
