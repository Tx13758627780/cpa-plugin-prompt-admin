package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cpa-plugin-prompt-admin/internal/config"
)

var sgt = time.FixedZone("SGT", 8*3600)

// Engine owns prompt-admin state, repo sync, and the resolved prompt map.
type Engine struct {
	cfg     config.Config
	dataDir string
	mu      sync.Mutex
	cond    *sync.Cond
	running bool
	closed  bool
	started string
	kind    string
	state   State
	status  Status
	book    *promptBook
	client  *http.Client
	now     func() time.Time
	fetch   func() modelSnap
	logMu   sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func Open(cfg config.Config) (*Engine, error) {
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "repos"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitconfig-empty"), nil, 0o600); err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:     cfg,
		dataDir: dir,
		now:     time.Now,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	e.cond = sync.NewCond(&e.mu)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.state = loadState(e.statePath())
	e.loadStatus()
	e.book = newPromptBook(e.promptsPath(), cfg.NamesFile)
	return e, nil
}

// Active reports whether injection and background refresh are enabled.
func (e *Engine) Active() bool {
	return e != nil && e.cfg.Enabled
}

// DataDir is the absolute directory holding prompts, repos, and backups.
func (e *Engine) DataDir() string {
	if e == nil {
		return ""
	}
	return e.dataDir
}

// Start launches the model-list watcher and the optional git sync timer.
func (e *Engine) Start() {
	if e == nil || !e.cfg.Enabled {
		return
	}
	e.once.Do(func() {
		e.wg.Add(1)
		go e.loop()
	})
}

func (e *Engine) Close() {
	if e == nil || e.cancel == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	e.cond.Broadcast()
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
}

func (e *Engine) loop() {
	defer e.wg.Done()
	if err := e.Apply("startup"); err != nil {
		e.logf("WARN startup apply: %v", err)
	}
	watch := time.NewTicker(e.cfg.WatchInterval)
	defer watch.Stop()
	var syncC <-chan time.Time
	if e.cfg.SyncInterval > 0 {
		ticker := time.NewTicker(e.cfg.SyncInterval)
		defer ticker.Stop()
		syncC = ticker.C
	}
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-watch.C:
			if e.isStale() {
				if err := e.Apply("auto:watch"); err != nil {
					e.logf("WARN auto refresh failed: %v", err)
				}
			}
		case <-syncC:
			if !e.SyncAsync(true, "timer") {
				e.logf("timer sync skipped; another sync is running")
			}
		}
	}
}

func (e *Engine) isStale() bool {
	e.mu.Lock()
	running := e.running
	prev := e.status.Sig
	e.mu.Unlock()
	if running {
		return false
	}
	return e.signature(e.fetchModels()) != prev
}

// Prompts returns the resolved prompt map and hidden old->current aliases.
// The map is reloaded when prompts.json or the names file changes on disk.
func (e *Engine) Prompts() (map[string]string, map[string]string) {
	if e == nil || e.book == nil {
		return map[string]string{}, map[string]string{}
	}
	return e.book.get()
}

func (e *Engine) Snapshot() Snapshot {
	if e.isStale() {
		_ = e.Apply("auto:page")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

func (e *Engine) snapshotLocked() Snapshot {
	repos := make([]RepoView, 0, len(e.state.Repos))
	for _, repo := range e.state.Repos {
		repos = append(repos, RepoView{
			ID: repo.ID, Kind: repo.Kind, URL: repo.URL, Branch: repo.Branch, Subpath: repo.Subpath,
			Enabled: repo.IsEnabled(), AddedAt: repo.AddedAt, HasToken: repo.Token != "",
		})
	}
	return Snapshot{
		Repos: repos, Overrides: e.state.Overrides, Local: e.state.Local, Choices: e.state.Choices,
		DefaultPrompt: e.state.DefaultPrompt, Status: e.status, Log: e.logTail(200),
		Sync: SyncInfo{Running: e.running, Started: e.started, Kind: e.kind},
	}
}

func (e *Engine) Apply(reason string) error {
	if err := e.acquire(kindOf(false)); err != nil {
		return err
	}
	defer e.release()
	return e.computeBody(false, reason)
}

func (e *Engine) SyncAsync(pull bool, reason string) bool {
	e.mu.Lock()
	if e.closed || e.running {
		e.mu.Unlock()
		return false
	}
	e.running = true
	e.kind = kindOf(pull)
	e.started = e.stamp()
	e.mu.Unlock()
	go func() {
		defer e.release()
		if err := e.computeBody(pull, reason); err != nil {
			e.logf("ERROR %s failed: %v", reason, err)
		}
	}()
	return true
}

func (e *Engine) queue(pull bool, reason string) {
	go func() {
		if err := e.acquire(kindOf(pull)); err != nil {
			return
		}
		defer e.release()
		if err := e.computeBody(pull, reason); err != nil {
			e.logf("ERROR %s failed: %v", reason, err)
		}
	}()
}

func kindOf(pull bool) string {
	if pull {
		return "sync"
	}
	return "apply"
}

func (e *Engine) acquire(kind string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.running && !e.closed {
		e.cond.Wait()
	}
	if e.closed {
		return errors.New("prompt-admin is shut down")
	}
	e.running = true
	e.kind = kind
	e.started = e.stamp()
	return nil
}

func (e *Engine) release() {
	e.mu.Lock()
	e.running = false
	e.cond.Broadcast()
	e.mu.Unlock()
}

func (e *Engine) ManualSync() (bool, string) {
	if e.SyncAsync(true, "manual") {
		return true, "已开始同步"
	}
	return false, "已有同步在进行"
}

func (e *Engine) ReadFile(key string) (string, bool) {
	e.mu.Lock()
	repos := map[string]Repo{}
	for _, repo := range e.state.Repos {
		repos[repoKey(repo)] = repo
	}
	e.mu.Unlock()
	return e.readRepoFile(repos, key)
}

func (e *Engine) AddRepo(rawURL, branch, subpath, token string) error {
	repo, err := parseSource(rawURL, branch, subpath, e.cfg.AllowFileURLs)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token != "" {
		if len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
			return UserError("Token 不合法")
		}
		repo.Token = token
	}
	enabled := true
	repo.Enabled = &enabled
	repo.AddedAt = e.stamp()
	e.mu.Lock()
	for _, existing := range e.state.Repos {
		if existing.ID == repo.ID {
			e.mu.Unlock()
			return UserError("这个仓库（同分支）已经添加过了")
		}
	}
	e.state.Repos = append(e.state.Repos, repo)
	err = saveState(e.statePath(), e.state)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	sub := repo.Subpath
	if sub == "" {
		sub = "/"
	}
	e.logf("repo added %s subpath=%s", fileLabel(repo, ""), sub)
	e.queue(true, "repos/add")
	return nil
}

func (e *Engine) RemoveRepo(id string) error {
	return e.mutate(false, "repos/remove", func(st *State) error {
		var found *Repo
		next := st.Repos[:0]
		for i := range st.Repos {
			if st.Repos[i].ID == id {
				copyRepo := st.Repos[i]
				found = &copyRepo
				continue
			}
			next = append(next, st.Repos[i])
		}
		if found == nil {
			return UserError("找不到仓库")
		}
		st.Repos = next
		_ = os.RemoveAll(filepath.Join(e.reposDir(), id))
		e.logf("repo removed %s", fileLabel(*found, ""))
		return nil
	})
}

func (e *Engine) MoveRepo(id, dir string) error {
	return e.mutate(false, "repos/move", func(st *State) error {
		index := -1
		for i := range st.Repos {
			if st.Repos[i].ID == id {
				index = i
				break
			}
		}
		delta := 1
		if dir == "up" {
			delta = -1
		}
		next := index + delta
		if index < 0 || next < 0 || next >= len(st.Repos) {
			return UserError("无法移动")
		}
		st.Repos[index], st.Repos[next] = st.Repos[next], st.Repos[index]
		return nil
	})
}

func (e *Engine) UpdateRepo(id string, enabled *bool, subpath *string, token *string) error {
	pull := false
	e.mu.Lock()
	st := e.state.clone()
	found := false
	for i := range st.Repos {
		if st.Repos[i].ID != id {
			continue
		}
		found = true
		if enabled != nil {
			value := *enabled
			st.Repos[i].Enabled = &value
		}
		if subpath != nil {
			sp := strings.Trim(strings.TrimSpace(*subpath), "/")
			for _, part := range strings.Split(sp, "/") {
				if part == ".." {
					e.mu.Unlock()
					return UserError("子目录不能包含 ..")
				}
			}
			st.Repos[i].Subpath = sp
		}
		if token != nil {
			tok := strings.TrimSpace(*token)
			if tok != "" {
				if len(tok) > 4096 || strings.ContainsAny(tok, "\r\n") {
					e.mu.Unlock()
					return UserError("Token 不合法")
				}
				st.Repos[i].Token = tok
			} else {
				st.Repos[i].Token = ""
			}
			pull = true
		}
		break
	}
	if !found {
		e.mu.Unlock()
		return UserError("找不到仓库")
	}
	if err := saveState(e.statePath(), st); err != nil {
		e.mu.Unlock()
		return err
	}
	e.state = st
	e.mu.Unlock()
	if pull {
		e.queue(true, "repos/update")
		return nil
	}
	return e.Apply("repos/update")
}

func (e *Engine) Choose(model, key string) error {
	return e.mutate(false, "choose", func(st *State) error {
		var cands []Candidate
		for _, conflict := range e.status.Conflicts {
			if conflict.Model == model {
				cands = conflict.Candidates
				break
			}
		}
		if len(cands) == 0 {
			for _, row := range e.status.Rows {
				if row.Model == model {
					cands = row.Candidates
					break
				}
			}
		}
		if !contains(candidateKeys(cands), key) {
			return UserError("该候选已不存在，请刷新")
		}
		st.Choices[model] = Choice{Key: key, Seen: candidateKeys(cands), At: e.stamp()}
		e.logf("conflict resolved model=%s keep=%s", model, key)
		return nil
	})
}

func (e *Engine) Override(model, kind, text, key string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return UserError("缺少模型名")
	}
	if err := checkText(text); err != nil {
		return err
	}
	return e.mutate(false, "override", func(st *State) error {
		switch kind {
		case "clear":
			delete(st.Overrides, model)
		case "text":
			st.Overrides[model] = Override{Type: "text", Text: text}
		case "file":
			st.Overrides[model] = Override{Type: "file", Key: key}
		case "none":
			st.Overrides[model] = Override{Type: "none"}
		default:
			return UserError("未知覆盖类型")
		}
		e.logf("override %s -> %s", model, kind)
		return nil
	})
}

func (e *Engine) SetLocal(model, text string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return UserError("缺少模型名")
	}
	if err := checkText(text); err != nil {
		return err
	}
	return e.mutate(false, "local", func(st *State) error {
		if strings.TrimSpace(text) != "" {
			st.Local[model] = text
			e.logf("本地提示词 saved for %s", model)
			return nil
		}
		delete(st.Local, model)
		e.logf("本地提示词 removed for %s", model)
		return nil
	})
}

func (e *Engine) SetDefault(text string) error {
	if err := checkText(text); err != nil {
		return err
	}
	return e.mutate(false, "default", func(st *State) error {
		st.DefaultPrompt = text
		if strings.TrimSpace(text) != "" {
			e.logf("default prompt set")
		} else {
			e.logf("default prompt cleared")
		}
		return nil
	})
}

func (e *Engine) mutate(pull bool, reason string, fn func(*State) error) error {
	e.mu.Lock()
	st := e.state.clone()
	err := fn(&st)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if err := saveState(e.statePath(), st); err != nil {
		e.mu.Unlock()
		return err
	}
	e.state = st
	e.mu.Unlock()
	if pull {
		e.queue(true, reason)
		return nil
	}
	return e.Apply(reason)
}

func checkText(text string) error {
	if len(text) > maxFileBytes {
		return UserError("提示词超过 256KB")
	}
	return nil
}

func (e *Engine) statePath() string   { return filepath.Join(e.dataDir, "state.json") }
func (e *Engine) statusPath() string  { return filepath.Join(e.dataDir, "status.json") }
func (e *Engine) promptsPath() string { return filepath.Join(e.dataDir, "prompts.json") }
func (e *Engine) reposDir() string    { return filepath.Join(e.dataDir, "repos") }
func (e *Engine) backupDir() string   { return filepath.Join(e.dataDir, "backups") }
func (e *Engine) logPath() string     { return filepath.Join(e.dataDir, "sync.log") }

func (e *Engine) loadStatus() {
	raw, err := os.ReadFile(e.statusPath())
	if err != nil {
		return
	}
	var status Status
	if err := json.Unmarshal(raw, &status); err == nil {
		e.status = status
	}
}

func (e *Engine) saveStatusLocked() error {
	return writeJSON(e.statusPath(), e.status, 0o600)
}

func (e *Engine) stamp() string {
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	return now.In(sgt).Format("2006-01-02 15:04:05 SGT")
}

func (e *Engine) logf(format string, args ...any) {
	line := e.stamp() + " " + fmt.Sprintf(format, args...)
	log.Printf("prompt-admin: %s", line)
	e.logMu.Lock()
	defer e.logMu.Unlock()
	f, err := os.OpenFile(e.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
	info, err := os.Stat(e.logPath())
	if err != nil || info.Size() <= 1_000_000 {
		return
	}
	lines := e.readLog()
	if len(lines) > 3000 {
		lines = lines[len(lines)-3000:]
	}
	_ = writeAtomic(e.logPath(), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func (e *Engine) logTail(n int) []string {
	lines := e.readLog()
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func (e *Engine) readLog() []string {
	f, err := os.Open(e.logPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

type promptBook struct {
	mu          sync.RWMutex
	prompts     map[string]string
	aliases     map[string]string
	promptsPath string
	namesPath   string
	promptSig   fileSig
	namesSig    fileSig
}

type fileSig struct {
	mtime int64
	size  int64
	ok    bool
}

func newPromptBook(promptsPath, namesPath string) *promptBook {
	book := &promptBook{
		prompts:     map[string]string{},
		aliases:     map[string]string{},
		promptsPath: promptsPath,
		namesPath:   namesPath,
	}
	book.reload()
	return book
}

func (b *promptBook) setPrompts(prompts map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompts = cloneMap(prompts)
	b.promptSig = statSig(b.promptsPath)
}

func (b *promptBook) get() (map[string]string, map[string]string) {
	b.reload()
	b.mu.RLock()
	defer b.mu.RUnlock()
	return cloneMap(b.prompts), cloneMap(b.aliases)
}

func (b *promptBook) reload() {
	promptSig := statSig(b.promptsPath)
	namesSig := statSig(b.namesPath)
	b.mu.Lock()
	defer b.mu.Unlock()
	if promptSig != b.promptSig {
		b.prompts = readPromptFile(b.promptsPath).Prompts
		b.promptSig = promptSig
	}
	if namesSig != b.namesSig {
		b.aliases = loadNames(b.namesPath).Aliases
		b.namesSig = namesSig
	}
}

func statSig(path string) fileSig {
	info, err := os.Stat(path)
	if err != nil {
		return fileSig{}
	}
	return fileSig{mtime: info.ModTime().UnixNano(), size: info.Size(), ok: true}
}
