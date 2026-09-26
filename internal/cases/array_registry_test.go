package cases

import (
	"errors"
	"sync"
	"testing"

	"rocalc/internal/array"
	"rocalc/internal/osmotic"
)

func sampleArraySpec(name string) ArraySpec {
	return ArraySpec{
		Name: name,
		Config: array.Config{
			Name: name,
			Feed: array.Feed{
				Solution: osmotic.Solution{Molarity: 0.1, Temperature: 298.15, VanTHoff: 2},
				Flow:     1000,
			},
			Stages: []array.Stage{
				{Area: 12, Permeability: 1.8, Rejection: 1, Applied: 12},
				{Area: 10, Permeability: 1.8, Rejection: 1, Applied: 11},
			},
		},
	}
}

func TestArrayRegistry_RegisterGetListDelete(t *testing.T) {
	r := NewArrayRegistry()
	if err := r.Register(sampleArraySpec("arr-a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(sampleArraySpec("arr-a")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重名应 ErrAlreadyExists，实际 %v", err)
	}
	if err := r.Register(sampleArraySpec("")); err == nil {
		t.Fatal("空名必须拒绝")
	}

	got, err := r.Get("arr-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Stages) != 2 || got.Stages[0].Area != 12 {
		t.Fatalf("取回的配置内容不符: %+v", got)
	}
	if names := r.List(); len(names) != 1 || names[0] != "arr-a" {
		t.Fatalf("列表不符: %v", names)
	}

	if _, err := r.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在应 ErrNotFound，实际 %v", err)
	}
	if err := r.Delete("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在应 ErrNotFound，实际 %v", err)
	}
	if err := r.Delete("arr-a"); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 0 {
		t.Fatal("删除后列表应为空")
	}
}

func TestArrayRegistry_PutReplacesAndCopiesIsolate(t *testing.T) {
	r := NewArrayRegistry()
	r.Put(sampleArraySpec("arr-x"))
	r.Put(sampleArraySpec("arr-x")) // upsert 不报错

	// 取回的是副本：改副本不得污染登记表。
	got, _ := r.Get("arr-x")
	got.Stages[0].Area = 999
	again, _ := r.Get("arr-x")
	if again.Stages[0].Area != 12 {
		t.Fatal("修改取回副本污染了登记表内的配置")
	}

	// 登记时也是拷贝：改原始入参不得影响已登记内容。
	spec := sampleArraySpec("arr-y")
	r.Put(spec)
	spec.Stages[0].Area = 777
	stored, _ := r.Get("arr-y")
	if stored.Stages[0].Area != 12 {
		t.Fatal("登记后修改原始入参影响了登记表内的配置")
	}
}

func TestArrayRegistry_ConcurrentAccess(t *testing.T) {
	r := NewArrayRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a'+i%26)) + "-arr"
			r.Put(sampleArraySpec(name))
			if _, err := r.Get(name); err != nil {
				t.Errorf("并发取配置失败: %v", err)
			}
			_ = r.List()
		}(i)
	}
	wg.Wait()
}
