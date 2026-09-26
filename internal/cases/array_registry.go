package cases

import (
	"fmt"
	"sort"
	"sync"

	"rocalc/internal/array"
	"rocalc/internal/validation"
)

// ArraySpec 是对外登记的多段串联阵列配置：一个具名的阵列膜参数。
// 阵列共用的进料工况不在配置内：点名核算时由所套用的工况档提供。
type ArraySpec struct {
	Name string
	array.Config
}

// ArrayRegistry 是线程安全的具名阵列配置登记表，
// 与单段工况档 Registry 完全分开，两套数据互不串。
type ArrayRegistry struct {
	mu    sync.RWMutex
	specs map[string]ArraySpec
}

// NewArrayRegistry 创建空的阵列配置登记表。
func NewArrayRegistry() *ArrayRegistry {
	return &ArrayRegistry{specs: make(map[string]ArraySpec)}
}

// Register 登记一份新阵列配置；同名已存在返回 ErrAlreadyExists。
func (r *ArrayRegistry) Register(spec ArraySpec) error {
	if spec.Name == "" {
		return validation.NewError(validation.CodeNonPositiveParameter, "阵列配置名称不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.specs[spec.Name]; ok {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, spec.Name)
	}
	r.specs[spec.Name] = cloneArraySpec(spec)
	return nil
}

// Put 登记或整体替换一份阵列配置（upsert 语义，对应 HTTP PUT）。
func (r *ArrayRegistry) Put(spec ArraySpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.Name] = cloneArraySpec(spec)
}

// Get 取一份阵列配置的副本；不存在返回 ErrNotFound。
func (r *ArrayRegistry) Get(name string) (ArraySpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[name]
	if !ok {
		return ArraySpec{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return cloneArraySpec(spec), nil
}

// List 返回按名字排序的已登记阵列配置名称。
func (r *ArrayRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.specs))
	for name := range r.specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Delete 删除一份阵列配置；不存在返回 ErrNotFound。
func (r *ArrayRegistry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.specs[name]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(r.specs, name)
	return nil
}

func cloneArraySpec(s ArraySpec) ArraySpec {
	cp := s
	if s.Stages != nil {
		cp.Stages = append([]array.Stage(nil), s.Stages...)
	}
	return cp
}
