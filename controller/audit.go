package controller

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// AuditEntry 是一条不可变审计记录（动作、拒绝、人工操作、故障等）。
type AuditEntry struct {
	Seq      int64                  `json:"seq"`
	TS       time.Time              `json:"ts"`
	Category string                 `json:"category"` // command|reject|manual|fault|peak|soc|lifecycle
	Reason   string                 `json:"reason"`
	Detail   string                 `json:"detail,omitempty"`
	Actor    string                 `json:"actor,omitempty"`
	Target   string                 `json:"target,omitempty"`
	Value    float64                `json:"value,omitempty"`
	DedupKey string                 `json:"dedup_key,omitempty"`
	Extra    map[string]interface{} `json:"extra,omitempty"`
}

// Journal 是顺序追加的审计日志。相同 DedupKey 的记录只落盘一次，
// 因此重启后重放历史事件不会重复写动作。
type Journal struct {
	mu   sync.Mutex
	w    io.Writer
	f    *os.File
	seq  int64
	seen map[string]int64
	all  []AuditEntry

	// replay 为 true 时 Append 只重建去重水位，不写盘（重启重放用）。
	replay bool
}

// SetReplay 开关重放模式。
func (j *Journal) SetReplay(on bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.replay = on
}

// NewJournal 创建审计日志；path 非空时持久化到文件（重启可 Load 续跑）。
func NewJournal(path string) (*Journal, error) {
	j := &Journal{seen: make(map[string]int64)}
	if path == "" {
		j.w = io.Discard
		return j, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	j.w = f
	return j, nil
}

// Append 追加一条记录。DedupKey 非空且已存在时跳过（返回已有序号）。
func (j *Journal) Append(e AuditEntry) (AuditEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.DedupKey != "" {
		if seq, ok := j.seen[e.DedupKey]; ok {
			for _, ex := range j.all {
				if ex.Seq == seq {
					return ex, false
				}
			}
		}
	}
	j.seq++
	e.Seq = j.seq
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	data, _ := json.Marshal(e)
	data = append(data, '\n')
	if !j.replay {
		_, _ = j.w.Write(data)
	}
	j.all = append(j.all, e)
	if e.DedupKey != "" {
		j.seen[e.DedupKey] = e.Seq
	}
	return e, true
}

// Entries 返回日志副本（按序号升序）。
func (j *Journal) Entries() []AuditEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]AuditEntry, len(j.all))
	copy(out, j.all)
	sort.Slice(out, func(i, k int) bool { return out[i].Seq < out[k].Seq })
	return out
}

// Close 关闭底层文件。
func (j *Journal) Close() error {
	if j.f != nil {
		return j.f.Close()
	}
	return nil
}

// LoadJournal 从 JSONL 文件读取审计记录（重启续跑/回放校验用）。
func LoadJournal(path string) ([]AuditEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("journal line %d: %w", line, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// PrimeFrom 在重放历史后重建去重索引与序号水位（重启续跑用）。
func (j *Journal) PrimeFrom(entries []AuditEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range entries {
		j.all = append(j.all, e)
		if e.Seq > j.seq {
			j.seq = e.Seq
		}
		if e.DedupKey != "" {
			j.seen[e.DedupKey] = e.Seq
		}
	}
	sort.Slice(j.all, func(i, k int) bool { return j.all[i].Seq < j.all[k].Seq })
}
