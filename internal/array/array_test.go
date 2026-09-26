package array

import (
	"errors"
	"math"
	"sync"
	"testing"

	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// trainConfig 返回一份三段串联配置：膜面积递减、逐段压差不同、部分截留。
func trainConfig() Config {
	return Config{
		Name: "train-3",
		Feed: osmotic.Solution{
			MassConcentration: 5.0,
			MolarMass:         58.44,
			Temperature:       298.15,
			VanTHoff:          2,
		},
		FeedFlow: 1000,
		Stages: []StageSpec{
			{Area: 36, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 12},
			{Area: 20, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 13},
			{Area: 12, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 13.5},
		},
	}
}

func codeOf(err error) string {
	var de *validation.DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// referenceMarch 是测试侧的独立参考推进：手工把上一段浓水拼进下一段进料，
// 逐段调单段内核。用来交叉验证 Evaluate 的段间接线没有接错。
func referenceMarch(t *testing.T, cfg Config) []membrane.Outcome {
	t.Helper()
	var outs []membrane.Outcome
	feed := cfg.Feed
	flow := cfg.FeedFlow
	for i, st := range cfg.Stages {
		out, err := membrane.Evaluate(membrane.FeedSpec{
			Feed:            feed,
			AppliedPressure: st.AppliedPressure,
			FeedFlow:        flow,
			Permeability:    st.Permeability,
			Area:            st.Area,
			Rejection:       st.Rejection,
			Polarization:    st.Polarization,
		})
		if err != nil {
			t.Fatalf("参考推进第 %d 段失败: %v", i+1, err)
		}
		outs = append(outs, *out)
		feed = osmotic.Solution{
			Molarity:    out.BrineMolarity,
			Temperature: cfg.Feed.Temperature,
			VanTHoff:    cfg.Feed.VanTHoff,
		}
		flow = out.BrineFlow
	}
	return outs
}

func TestEvaluate_ThreeStages_HandMarch(t *testing.T) {
	cfg := trainConfig()
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("三段串联应跑通: %v", err)
	}
	if res.Status != StatusOK || res.Failure != nil || res.Summary == nil {
		t.Fatalf("跑通时应 status=ok、无 failure、有 summary，实际 %+v", res)
	}
	if len(res.Stages) != 3 {
		t.Fatalf("应返回 3 段中间结果，实际 %d", len(res.Stages))
	}

	// ---- 第 1 段：从第一性原理手算对照（π=i·C·R·T、NDP、Qp、Cb）----
	const R = 0.08314
	cf1 := 5.0 / 58.44
	piF1 := 2 * cf1 * R * 298.15
	cp1 := cf1 * (1 - 0.98)
	piP1 := 2 * cp1 * R * 298.15
	ndp1 := 12 - piF1 + piP1
	qp1 := 36 * 1.8 * ndp1
	y1 := qp1 / 1000
	cb1 := cf1 * (1 - 0.02*y1) / (1 - y1)
	s1 := res.Stages[0].Out
	if math.Abs(s1.FeedMolarity-cf1) > 1e-12 {
		t.Fatalf("第 1 段进料浓度 %.12g ≠ 手算 %.12g", s1.FeedMolarity, cf1)
	}
	if math.Abs(s1.NetDrivingPressure-ndp1) > 1e-9 {
		t.Fatalf("第 1 段 NDP %.12g ≠ 手算 %.12g", s1.NetDrivingPressure, ndp1)
	}
	if math.Abs(s1.PermeateFlow-qp1) > 1e-6 {
		t.Fatalf("第 1 段产水量 %.12g ≠ 手算 %.12g", s1.PermeateFlow, qp1)
	}
	if math.Abs(s1.BrineMolarity-cb1) > 1e-9 {
		t.Fatalf("第 1 段浓水浓度 %.12g ≠ 手算 %.12g", s1.BrineMolarity, cb1)
	}

	// ---- 全部段：与测试侧独立参考推进逐段对账 ----
	refs := referenceMarch(t, cfg)
	for i, ref := range refs {
		got := res.Stages[i].Out
		if got.BrineMolarity != ref.BrineMolarity || got.PermeateFlow != ref.PermeateFlow {
			t.Fatalf("第 %d 段结果与参考推进不符：Cb %g vs %g，Qp %g vs %g",
				i+1, got.BrineMolarity, ref.BrineMolarity, got.PermeateFlow, ref.PermeateFlow)
		}
	}

	// ---- 汇总：总产水量、总回收率必须能用逐段结果加总验证 ----
	sum := res.Summary
	var sumQp float64
	for _, s := range res.Stages {
		sumQp += s.Out.PermeateFlow
	}
	if math.Abs(sum.TotalPermeateFlow-sumQp) > 1e-9 {
		t.Fatalf("汇总产水量 %g ≠ 逐段加总 %g", sum.TotalPermeateFlow, sumQp)
	}
	if math.Abs(sum.OverallRecovery-sumQp/cfg.FeedFlow) > 1e-12 {
		t.Fatalf("汇总回收率 %g ≠ %g", sum.OverallRecovery, sumQp/cfg.FeedFlow)
	}
	if sum.FinalBrineMolarity != res.Stages[2].Out.BrineMolarity {
		t.Fatal("末段浓水浓度应与第 3 段一致")
	}
	if sum.FinalBrineFlow != res.Stages[2].Out.BrineFlow {
		t.Fatal("末段浓水流量应与第 3 段一致")
	}
}

func TestEvaluate_BrineFeedsNextStage(t *testing.T) {
	// 段间接线：第 k+1 段的进料浓度/流量必须逐项等于第 k 段的浓水。
	res, err := Evaluate(trainConfig())
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k+1 < len(res.Stages); k++ {
		prev, next := res.Stages[k], res.Stages[k+1]
		if next.Out.FeedMolarity != prev.Out.BrineMolarity {
			t.Fatalf("第 %d 段进料浓度 %g ≠ 第 %d 段浓水浓度 %g",
				next.Index, next.Out.FeedMolarity, prev.Index, prev.Out.BrineMolarity)
		}
		if next.Spec.FeedFlow != prev.Out.BrineFlow {
			t.Fatalf("第 %d 段进料流量 %g ≠ 第 %d 段浓水流量 %g",
				next.Index, next.Spec.FeedFlow, prev.Index, prev.Out.BrineFlow)
		}
		if next.Spec.Feed.Temperature != prev.Spec.Feed.Temperature ||
			next.Spec.Feed.VanTHoff != prev.Spec.Feed.VanTHoff {
			t.Fatal("温度与范特霍夫因子应随阵列级工况逐段不变")
		}
	}
}

func TestEvaluate_StageFailure_PreservesPrefix(t *testing.T) {
	// 中间段压差（1 bar）远低于该段实际进料渗透压（约 8.5 bar）：
	// 必须定位到第 2 段，且第 1 段结果原样保留。
	cfg := trainConfig()
	cfg.Stages[1].AppliedPressure = 1
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("段非法不应作为 error 抛出: %v", err)
	}
	if res.Status != StatusStageFailed {
		t.Fatalf("状态应为 stage_failed，实际 %s", res.Status)
	}
	if res.Failure == nil {
		t.Fatal("失败时 Failure 不得为空")
	}
	if res.Failure.StageIndex != 2 {
		t.Fatalf("应定位到第 2 段，实际第 %d 段", res.Failure.StageIndex)
	}
	if res.Failure.Code != validation.CodeNonPositiveNDP {
		t.Fatalf("错误码应为 non_positive_ndp，实际 %s", res.Failure.Code)
	}
	if len(res.Stages) != 1 {
		t.Fatalf("失败段之前应保留 1 段结果，实际 %d 段", len(res.Stages))
	}
	if res.Summary != nil {
		t.Fatal("未跑完全部段不得给汇总")
	}

	// 前缀段结果与正常跑通时完全一致，不被后面的失败污染。
	ok, err := Evaluate(trainConfig())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stages[0].Out != ok.Stages[0].Out {
		t.Fatal("失败段之前的结果必须与正常跑通时逐位一致")
	}

	// 失败段的进料条件 = 第 1 段浓水接力过来的工况。
	if res.Failure.Feed.Molarity != res.Stages[0].Out.BrineMolarity {
		t.Fatalf("失败段进料浓度 %g 应等于第 1 段浓水 %g",
			res.Failure.Feed.Molarity, res.Stages[0].Out.BrineMolarity)
	}
	if res.Failure.Feed.Flow != res.Stages[0].Out.BrineFlow {
		t.Fatalf("失败段进料流量 %g 应等于第 1 段浓水流量 %g",
			res.Failure.Feed.Flow, res.Stages[0].Out.BrineFlow)
	}
	if res.Failure.Feed.AppliedPressure != 1 {
		t.Fatal("失败段快照应带上该段设定的工作压差")
	}
	if res.Failure.Feed.OsmoticPressure <= 1 {
		t.Fatalf("失败段进料渗透压应明显高于 1 bar 的压差，实际 %g",
			res.Failure.Feed.OsmoticPressure)
	}
}

func TestEvaluate_MissingStageParamFails(t *testing.T) {
	// 段参数不沿用上一段：第 3 段膜面积缺省（零值）必须在该段
	// 被校验拒绝并带段号，而不是静默算出异常结果。
	cfg := trainConfig()
	cfg.Stages[2].Area = 0
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("段参数缺失不应作为 error 抛出: %v", err)
	}
	if res.Status != StatusStageFailed || res.Failure == nil {
		t.Fatalf("应 stage_failed，实际 %+v", res)
	}
	if res.Failure.StageIndex != 3 {
		t.Fatalf("应定位到第 3 段，实际第 %d 段", res.Failure.StageIndex)
	}
	if res.Failure.Code != validation.CodeNonPositiveParameter {
		t.Fatalf("错误码应为 non_positive_parameter，实际 %s", res.Failure.Code)
	}
	if len(res.Stages) != 2 {
		t.Fatalf("前两段结果应保留，实际 %d 段", len(res.Stages))
	}
}

func TestEvaluate_SingleStage_MatchesKernel(t *testing.T) {
	// 段数为 1：多段编排的结果必须与单段内核对同一份工况的输出逐位一致。
	cfg := trainConfig()
	cfg.Stages = cfg.Stages[:1]
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || len(res.Stages) != 1 {
		t.Fatalf("单段应跑通，实际 %+v", res)
	}
	want, err := membrane.Evaluate(membrane.FeedSpec{
		Feed:            cfg.Feed,
		AppliedPressure: 12,
		FeedFlow:        cfg.FeedFlow,
		Permeability:    1.8,
		Area:            36,
		Rejection:       0.98,
		Polarization:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stages[0].Out != *want {
		t.Fatalf("单段阵列结果与内核直接核算不一致：\n阵列 %+v\n内核 %+v", res.Stages[0].Out, *want)
	}
	// 汇总也要对得上：总产水 = 唯一一段的产水，总回收率 = 该段回收率。
	if res.Summary.TotalPermeateFlow != want.PermeateFlow ||
		res.Summary.OverallRecovery != want.Recovery {
		t.Fatal("单段汇总应与该段结果一致")
	}
}

func TestEvaluate_InvalidStageCount(t *testing.T) {
	cfg := trainConfig()
	cfg.Stages = nil
	if _, err := Evaluate(cfg); codeOf(err) != validation.CodeInvalidStageCount {
		t.Fatalf("空段列表应报 invalid_stage_count，实际 %v", err)
	}
	cfg.Stages = []StageSpec{}
	if _, err := Evaluate(cfg); codeOf(err) != validation.CodeInvalidStageCount {
		t.Fatalf("零段应报 invalid_stage_count，实际 %v", err)
	}
}

func TestEvaluate_GlobalSaltBalance(t *testing.T) {
	// 全局账：阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量。
	// 用返回的逐段结果独立复算，残差必须落在浮点噪声内。
	res, err := Evaluate(trainConfig())
	if err != nil {
		t.Fatal(err)
	}
	sum := res.Summary

	var permMol float64
	for _, s := range res.Stages {
		permMol += s.Out.PermeateMolarity * s.Out.PermeateFlow
	}
	feedMol := res.Stages[0].Out.FeedMolarity * 1000
	brineMol := res.Stages[2].Out.BrineMolarity * res.Stages[2].Out.BrineFlow
	residual := feedMol - permMol - brineMol
	if math.Abs(residual) > 1e-9 {
		t.Fatalf("全局盐量不守恒：%g ≠ %g + %g（残差 %g）", feedMol, permMol, brineMol, residual)
	}
	if math.Abs(sum.Salt.ResidualMolFlow) > 1e-9 {
		t.Fatalf("汇总残差 %g 超浮点噪声", sum.Salt.ResidualMolFlow)
	}
	// 质量口径：进料 5 g/L × 1000 L/h = 5000 g/h。
	if math.Abs(sum.Salt.FeedSaltMassFlow-5000) > 1e-6 {
		t.Fatalf("进料盐量应为 5000 g/h，实际 %g", sum.Salt.FeedSaltMassFlow)
	}
	if math.Abs(sum.Salt.ResidualMassFlow) > 1e-6 {
		t.Fatalf("质量口径残差 %g 超浮点噪声", sum.Salt.ResidualMassFlow)
	}
}

func TestEvaluate_BlendedPermeateIsFlowWeighted(t *testing.T) {
	// 混合产水浓度必须按各段产水量加权，而不是算术平均。
	res, err := Evaluate(trainConfig())
	if err != nil {
		t.Fatal(err)
	}
	var permMol, totalQp, arith float64
	for _, s := range res.Stages {
		permMol += s.Out.PermeateMolarity * s.Out.PermeateFlow
		totalQp += s.Out.PermeateFlow
		arith += s.Out.PermeateMolarity
	}
	arith /= float64(len(res.Stages))
	want := permMol / totalQp
	if math.Abs(res.Summary.BlendedPermeateMolarity-want) > 1e-12 {
		t.Fatalf("混合产水浓度 %g ≠ 流量加权 %g", res.Summary.BlendedPermeateMolarity, want)
	}
	if math.Abs(res.Summary.BlendedPermeateMolarity-arith) < 1e-9 {
		t.Fatalf("混合产水浓度 %g 退化成了算术平均 %g", res.Summary.BlendedPermeateMolarity, arith)
	}
}

func TestRegistry_Lifecycle(t *testing.T) {
	r := NewRegistry()
	cfg := trainConfig()
	if err := r.Register(cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(cfg); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重名应报 ErrAlreadyExists，实际 %v", err)
	}
	got, err := r.Get(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	// 副本隔离：改取回的副本不得污染登记表。
	got.Stages[0].Area = 9999
	again, err := r.Get(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if again.Stages[0].Area != 36 {
		t.Fatal("登记表内配置被副本改动污染")
	}
	if names := r.List(); len(names) != 1 || names[0] != cfg.Name {
		t.Fatalf("列表异常: %v", names)
	}
	res, err := r.Evaluate(cfg.Name)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("点名评估失败: %v", err)
	}
	if err := r.Delete(cfg.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(cfg.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应报 ErrNotFound，实际 %v", err)
	}
}

func TestRegistry_ConcurrentAlternatingEvalsStayIndependent(t *testing.T) {
	// 多份不同配置交替/并发评估：结果各自稳定、互不串数据。
	r := NewRegistry()
	a := trainConfig()
	a.Name = "train-a"
	b := trainConfig()
	b.Name = "train-b"
	b.FeedFlow = 1200
	b.Stages[2].Area = 10
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(b); err != nil {
		t.Fatal(err)
	}
	baseA, err := r.Evaluate("train-a")
	if err != nil {
		t.Fatal(err)
	}
	baseB, err := r.Evaluate("train-b")
	if err != nil {
		t.Fatal(err)
	}
	if baseA.Summary.TotalPermeateFlow == baseB.Summary.TotalPermeateFlow {
		t.Fatal("两份不同配置的产水量不应相同（测试前提不成立）")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name, base := "train-a", baseA
			if i%2 == 1 {
				name, base = "train-b", baseB
			}
			for j := 0; j < 25; j++ {
				res, err := r.Evaluate(name)
				if err != nil {
					t.Errorf("评估 %s 失败: %v", name, err)
					return
				}
				if res.Summary.TotalPermeateFlow != base.Summary.TotalPermeateFlow ||
					res.Stages[0].Out != base.Stages[0].Out {
					t.Errorf("配置 %s 的结果被串改", name)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestBuiltinTrain_Evaluates(t *testing.T) {
	res, err := NewDefaultRegistry().Evaluate(BuiltinTrainName)
	if err != nil {
		t.Fatalf("内置三段配置必须可直接核算: %v", err)
	}
	if res.Status != StatusOK || len(res.Stages) != 3 {
		t.Fatalf("内置配置应三段跑通，实际 %+v", res)
	}
	if math.Abs(res.Summary.Salt.ResidualMolFlow) > 1e-9 {
		t.Fatalf("内置配置全局盐守恒残差 %g 超容差", res.Summary.Salt.ResidualMolFlow)
	}
	t.Logf("内置三段：总产水 %.2f L/h，总回收 %.4f，末段浓水 %.5f mol/L，混合产水 %.6f mol/L",
		res.Summary.TotalPermeateFlow, res.Summary.OverallRecovery,
		res.Summary.FinalBrineMolarity, res.Summary.BlendedPermeateMolarity)
}
