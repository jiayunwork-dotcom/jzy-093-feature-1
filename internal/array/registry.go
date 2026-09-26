package array

import (
	"fmt"
	"sort"
	"sync"

	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// Registry 是线程安全的具名多段配置登记表。
//
// 与工况档登记表同一原则：不缓存任何计算结果，取用一律返回副本，
// 不同配置之间、配置与评估结果之间互不串数据。
type Registry struct {
	mu   sync.RWMutex
	cfgs map[string]Config
}

// NewRegistry 创建空登记表。
func NewRegistry() *Registry {
	return &Registry{cfgs: make(map[string]Config)}
}

var (
	// ErrAlreadyExists 重复登记同名配置时返回。
	ErrAlreadyExists = fmt.Errorf("array config already exists")
	// ErrNotFound 点名了不存在的配置时返回。
	ErrNotFound = fmt.Errorf("array config not found")
)

// Register 登记一份新配置；同名已存在返回 ErrAlreadyExists。
func (r *Registry) Register(cfg Config) error {
	if cfg.Name == "" {
		return validation.NewError(validation.CodeNonPositiveParameter, "多段配置名称不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cfgs[cfg.Name]; ok {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, cfg.Name)
	}
	r.cfgs[cfg.Name] = cloneConfig(cfg)
	return nil
}

// Put 登记或整体替换一份配置（upsert 语义，对应 HTTP PUT）。
func (r *Registry) Put(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgs[cfg.Name] = cloneConfig(cfg)
}

// Get 取一份配置的副本。副本与登记表内部数据互不共享（段列表深拷贝），
// 调用方的任何改动都不会污染已登记配置。
func (r *Registry) Get(name string) (Config, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.cfgs[name]
	if !ok {
		return Config{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return cloneConfig(cfg), nil
}

// List 返回按名字排序的已登记配置名称。
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.cfgs))
	for name := range r.cfgs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Delete 删除一份配置；不存在返回 ErrNotFound。
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cfgs[name]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(r.cfgs, name)
	return nil
}

// Evaluate 点名一份已登记配置，取其副本独立完成一次阵列核算。
func (r *Registry) Evaluate(name string) (*Result, error) {
	cfg, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	return Evaluate(cfg)
}

// WithFeed 返回一份把阵列级进料整体替换为给定工况的配置副本：
//
// 对已登记工况档套用已登记多段配置时，工况档的溶液与进料流量整体
// 生效，配置自带的阵列级进料被忽略；段列表（各段膜参数与压差）
// 原样保留。这是唯一的覆盖规则，优先级：显式给定的进料 > 配置自带进料。
func WithFeed(cfg Config, feed osmotic.Solution, feedFlow float64) Config {
	cp := cloneConfig(cfg)
	cp.Feed = feed
	cp.FeedFlow = feedFlow
	return cp
}

func cloneConfig(c Config) Config {
	cp := c
	cp.Stages = append([]StageSpec(nil), c.Stages...)
	return cp
}
