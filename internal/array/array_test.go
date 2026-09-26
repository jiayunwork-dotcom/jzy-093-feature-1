package array

import (
	"errors"
	"math"
	"testing"

	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// threeStageConfig 是验收用的三段递减膜面积、逐段压差各异的阵列：
// 进料 5 g/L NaCl（25 ℃，i=2），Qf=1000 L/h，完全截留、无极化。
// 段面积 12/10/8 m²，段压差 12/11/10 bar，Lp 均 1.8 LMH/bar。
func threeStageConfig() Config {
	return Config{
		Name: "three-stage",
		Feed: Feed{
			Solution: osmotic.Solution{
				MassConcentration: 5.0,
				MolarMass:         58.44,
				Temperature:       298.15,
				VanTHoff:          2,
			},
			Flow: 1000,
		},
		Stages: []Stage{
			{Area: 12, Permeability: 1.8, Rejection: 1, Polarization: 1, Applied: 12},
			{Area: 10, Permeability: 1.8, Rejection: 1, Polarization: 1, Applied: 11},
			{Area: 8, Permeability: 1.8, Rejection: 1, Polarization: 1, Applied: 10},
		},
	}
}

// handStage 是测试内的独立手算：不走 membrane.Evaluate，
// 直接按公式 π=i·C·R·T、NDP=Δp−β·πf+πp、Qp=A·Lp·NDP、Cb=Cf/(1−Y) 推一段，
// 用来对照编排层逐段结果（公式写错时两边不会同时错成一样）。
type handStageResult struct {
	piF, ndp, qp, qb, y, cb, cp float64
}

func handStage(cf, qf, dp, area, lp, r, beta, temp, vanthoff float64) handStageResult {
	piF := vanthoff * cf * osmotic.R * temp
	cp := 0.0
	if !membrane.IsFullRejection(r) {
		cp = cf * (1 - r)
	}
	piP := vanthoff * cp * osmotic.R * temp
	ndp := dp - beta*piF + piP
	qp := area * lp * ndp
	y := qp / qf
	qb := qf - qp
	var cb float64
	if membrane.IsFullRejection(r) {
		cb = cf / (1 - y)
	} else {
		cb = cf * (1 - (1-r)*y) / (1 - y)
	}
	return handStageResult{piF: piF, ndp: ndp, qp: qp, qb: qb, y: y, cb: cb, cp: cp}
}

func codeOfErr(err error) string {
	var de *validation.DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// handFeedMolarity 以运行时浮点除法复算进料摩尔浓度。
// 注意不能写成常量表达式 5.0/58.44：常量任意精度求值与运行时
// float64 除法可能差 1 ulp，与服务实算对不上。
func handFeedMolarity() float64 {
	massConc, molarMass := 5.0, 58.44
	return massConc / molarMass
}

func TestEvaluateArray_HandCalcThreeStages(t *testing.T) {
	cfg := threeStageConfig()
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("配置应合法: %v", err)
	}
	if res.Failed != nil {
		t.Fatalf("三段应全部跑通，却在第 %d 段失败: %s", res.Failed.Stage, res.Failed.Reason)
	}
	if len(res.Stages) != 3 {
		t.Fatalf("应返回 3 段中间结果，实际 %d", len(res.Stages))
	}

	// 手工按段推进：上一段浓水 = 下一段进料。
	cf := handFeedMolarity()
	qf := 1000.0
	var totalQp float64
	for i, st := range cfg.Stages {
		want := handStage(cf, qf, st.Applied, st.Area, st.Permeability, st.Rejection, 1, 298.15, 2)
		got := res.Stages[i]

		if got.Stage != i+1 {
			t.Fatalf("段号应从 1 开始递增，第 %d 个结果段号为 %d", i, got.Stage)
		}
		if got.Feed.Molarity != cf || got.Feed.Flow != qf {
			t.Fatalf("第 %d 段进料应为上段浓水（%g mol/L, %g L/h），实际（%g, %g）",
				i+1, cf, qf, got.Feed.Molarity, got.Feed.Flow)
		}
		o := got.Outcome
		if math.Abs(o.BrineMolarity-want.cb) > 1e-12 {
			t.Fatalf("第 %d 段浓水浓度 %.12g 与手算 %.12g 不符", i+1, o.BrineMolarity, want.cb)
		}
		if math.Abs(o.PermeateFlow-want.qp) > 1e-9 {
			t.Fatalf("第 %d 段产水量 %.12g 与手算 %.12g 不符", i+1, o.PermeateFlow, want.qp)
		}
		if math.Abs(o.NetDrivingPressure-want.ndp) > 1e-12 {
			t.Fatalf("第 %d 段 NDP %.12g 与手算 %.12g 不符", i+1, o.NetDrivingPressure, want.ndp)
		}
		if math.Abs(o.BrineFlow-want.qb) > 1e-9 {
			t.Fatalf("第 %d 段浓水流量 %.12g 与手算 %.12g 不符", i+1, o.BrineFlow, want.qb)
		}

		totalQp += want.qp
		cf, qf = want.cb, want.qb // 段间衔接
	}

	// 汇总必须能用逐段结果加总验证。
	s := res.Summary
	if s == nil {
		t.Fatal("全部成功时必须有汇总")
	}
	if math.Abs(s.TotalPermeateFlow-totalQp) > 1e-9 {
		t.Fatalf("汇总产水量 %g ≠ 逐段加总 %g", s.TotalPermeateFlow, totalQp)
	}
	if math.Abs(s.OverallRecovery-totalQp/1000) > 1e-12 {
		t.Fatalf("总回收率 %g ≠ ΣQp/Qf1=%g", s.OverallRecovery, totalQp/1000)
	}
	if math.Abs(s.FinalBrineMolarity-cf) > 1e-12 || math.Abs(s.FinalBrineFlow-qf) > 1e-9 {
		t.Fatalf("末段浓水（%g mol/L, %g L/h）与手算末段（%g, %g）不符",
			s.FinalBrineMolarity, s.FinalBrineFlow, cf, qf)
	}
}

func TestEvaluateArray_GlobalSaltBalance(t *testing.T) {
	// 全局账：首段进料盐量 = 各段产水盐量之和 + 末段浓水盐量。
	// 用部分截留（产水带盐）的配置，让产水盐量非零，账才算真的被检验。
	cfg := threeStageConfig()
	for i := range cfg.Stages {
		cfg.Stages[i].Rejection = 0.95
	}
	res, err := Evaluate(cfg)
	if err != nil || res.Failed != nil {
		t.Fatalf("应全部跑通: err=%v failed=%v", err, res.Failed)
	}
	g := res.Summary.GlobalSalt

	// 摩尔口径：残差落在浮点噪声内。
	if math.Abs(g.ResidualMolar) > 1e-9 {
		t.Fatalf("全局摩尔盐量残差 %g mol/h 超浮点噪声", g.ResidualMolar)
	}
	// 独立复算：不用服务返回的汇总字段，自己从逐段结果加总。
	cf := handFeedMolarity()
	feedMol := cf * 1000
	var permMol float64
	for _, st := range res.Stages {
		permMol += st.Outcome.PermeateMolarity * st.Outcome.PermeateFlow
	}
	last := res.Stages[len(res.Stages)-1].Outcome
	brineMol := last.BrineMolarity * last.BrineFlow
	if math.Abs(feedMol-permMol-brineMol) > 1e-9 {
		t.Fatalf("独立复算全局盐账不闭合：%g ≠ %g + %g", feedMol, permMol, brineMol)
	}
	// 质量口径（给了摩尔质量）。
	if math.Abs(g.ResidualMass) > 1e-6 {
		t.Fatalf("全局质量盐量残差 %g g/h 超浮点噪声", g.ResidualMass)
	}
	if math.Abs(g.FeedMassFlow-5.0*1000) > 1e-6 {
		t.Fatalf("首段进料盐质量流量应为 5000 g/h，实际 %g", g.FeedMassFlow)
	}
}

func TestEvaluateArray_StageFailureKeepsPriorStages(t *testing.T) {
	// 第 2 段压差 4 bar，低于该段实际进料渗透压（约 5.09 bar）：
	// 必须定位到第 2 段，且第 1 段的中间结果原样保留。
	cfg := threeStageConfig()
	cfg.Stages[1].Applied = 4

	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("段失败不应作为 Go error 抛出，实际 %v", err)
	}
	if res.Failed == nil {
		t.Fatal("第 2 段 NDP 为负，必须报失败")
	}
	if res.Failed.Stage != 2 {
		t.Fatalf("应定位到第 2 段，实际第 %d 段", res.Failed.Stage)
	}
	if res.Failed.Code != validation.CodeNonPositiveNDP {
		t.Fatalf("错误码应为 non_positive_ndp，实际 %s", res.Failed.Code)
	}
	// 失败段进料条件 = 第 1 段浓水。
	if len(res.Stages) != 1 {
		t.Fatalf("失败前应有且仅有第 1 段结果，实际 %d 段", len(res.Stages))
	}
	first := res.Stages[0]
	if first.Stage != 1 || first.Outcome == nil {
		t.Fatal("第 1 段中间结果必须原样保留")
	}
	if res.Failed.Feed.Molarity != first.Outcome.BrineMolarity ||
		res.Failed.Feed.Flow != first.Outcome.BrineFlow {
		t.Fatalf("失败段进料（%g, %g）应等于第 1 段浓水（%g, %g）",
			res.Failed.Feed.Molarity, res.Failed.Feed.Flow,
			first.Outcome.BrineMolarity, first.Outcome.BrineFlow)
	}
	// 与手算对照：第 1 段结果不因后面失败而失真。
	want := handStage(handFeedMolarity(), 1000, 12, 12, 1.8, 1, 1, 298.15, 2)
	if math.Abs(first.Outcome.PermeateFlow-want.qp) > 1e-9 ||
		math.Abs(first.Outcome.BrineMolarity-want.cb) > 1e-12 {
		t.Fatal("第 1 段中间结果与手算不符")
	}
	if res.Summary != nil {
		t.Fatal("有段失败时不得给出全阵列汇总")
	}
}

func TestEvaluateArray_SingleStageMatchesSingleEvaluate(t *testing.T) {
	// 段数为 1：多段编排结果必须与单段内核逐位一致。
	cfg := threeStageConfig()
	cfg.Stages = cfg.Stages[:1]
	res, err := Evaluate(cfg)
	if err != nil || res.Failed != nil {
		t.Fatalf("单段阵列应跑通: err=%v failed=%v", err, res.Failed)
	}

	single, err := membrane.Evaluate(membrane.FeedSpec{
		Feed:            cfg.Feed.Solution,
		AppliedPressure: cfg.Stages[0].Applied,
		FeedFlow:        cfg.Feed.Flow,
		Permeability:    cfg.Stages[0].Permeability,
		Area:            cfg.Stages[0].Area,
		Rejection:       cfg.Stages[0].Rejection,
		Polarization:    cfg.Stages[0].Polarization,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Stages[0].Outcome
	if *got != *single {
		t.Fatalf("单段阵列结果必须与单段核算逐位一致:\n阵列 %+v\n单段 %+v", *got, *single)
	}
}

func TestEvaluateArray_InvalidStageCount(t *testing.T) {
	cfg := threeStageConfig()
	cfg.Stages = nil
	if _, err := Evaluate(cfg); codeOfErr(err) != validation.CodeInvalidStageCount {
		t.Fatalf("零段应报 invalid_stage_count，实际 %v", err)
	}
	declared := func(v int) *int { return &v }
	if err := ValidateConfig(threeStageConfig(), declared(0)); codeOfErr(err) != validation.CodeInvalidStageCount {
		t.Fatalf("显式声明 0 段应报 invalid_stage_count，实际 %v", err)
	}
	if err := ValidateConfig(threeStageConfig(), declared(-1)); codeOfErr(err) != validation.CodeInvalidStageCount {
		t.Fatalf("负声明段数应报 invalid_stage_count，实际 %v", err)
	}
	if err := ValidateConfig(threeStageConfig(), declared(2)); codeOfErr(err) != validation.CodeInvalidStageCount {
		t.Fatalf("声明段数与实际不符应报 invalid_stage_count，实际 %v", err)
	}
	if err := ValidateConfig(threeStageConfig(), declared(3)); err != nil {
		t.Fatalf("声明段数一致应通过，实际 %v", err)
	}
	if err := ValidateConfig(threeStageConfig(), nil); err != nil {
		t.Fatalf("未声明段数应通过，实际 %v", err)
	}
}

func TestEvaluateArray_MissingStageParams(t *testing.T) {
	// 缺参规则：膜面积/渗透系数/截留率/工作压差必须逐段给全，不沿用上一段。
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"第2段缺膜面积", func(c *Config) { c.Stages[1].Area = 0 }},
		{"第3段缺渗透系数", func(c *Config) { c.Stages[2].Permeability = 0 }},
		{"第1段缺截留率", func(c *Config) { c.Stages[0].Rejection = 0 }},
		{"第2段缺工作压差", func(c *Config) { c.Stages[1].Applied = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := threeStageConfig()
			tc.mutate(&cfg)
			if _, err := Evaluate(cfg); codeOfErr(err) != validation.CodeMissingStageParameter {
				t.Fatalf("应报 missing_stage_parameter，实际 %v", err)
			}
		})
	}
}

func TestEvaluateArray_WeightedPermeateMolarity(t *testing.T) {
	// 各段产水混合浓度必须按产水量加权，不等于各段浓度的算术平均。
	cfg := threeStageConfig()
	for i := range cfg.Stages {
		cfg.Stages[i].Rejection = 0.9
	}
	res, err := Evaluate(cfg)
	if err != nil || res.Failed != nil {
		t.Fatalf("应全部跑通: err=%v failed=%v", err, res.Failed)
	}
	var sumCpQp, sumQp, sumCp float64
	for _, st := range res.Stages {
		sumCpQp += st.Outcome.PermeateMolarity * st.Outcome.PermeateFlow
		sumQp += st.Outcome.PermeateFlow
		sumCp += st.Outcome.PermeateMolarity
	}
	want := sumCpQp / sumQp
	if math.Abs(res.Summary.WeightedPermeateMolar-want) > 1e-12 {
		t.Fatalf("加权产水浓度 %g 与 Σ(Cp·Qp)/ΣQp=%g 不符", res.Summary.WeightedPermeateMolar, want)
	}
	naive := sumCp / float64(len(res.Stages))
	if math.Abs(res.Summary.WeightedPermeateMolar-naive) < 1e-9 {
		t.Fatalf("各段流量不同，加权浓度 %g 不应等于算术平均 %g", res.Summary.WeightedPermeateMolar, naive)
	}
}

func TestEvaluateArray_StageOverridesApplyLocally(t *testing.T) {
	// 优先级规则：段级温度覆盖 > 阵列共用温度，且只影响该段、不向后传播。
	cfg := threeStageConfig()
	cfg.Stages[1].TemperatureK = 318.15 // 仅第 2 段升温
	res, err := Evaluate(cfg)
	if err != nil || res.Failed != nil {
		t.Fatalf("应全部跑通: err=%v failed=%v", err, res.Failed)
	}
	if res.Stages[0].Feed.Temperature != 298.15 || res.Stages[2].Feed.Temperature != 298.15 {
		t.Fatal("未覆盖的段必须用阵列共用温度")
	}
	if res.Stages[1].Feed.Temperature != 318.15 {
		t.Fatal("第 2 段必须采用段级覆盖温度")
	}
	// 手算对照第 2 段：用覆盖后的温度。
	cf := handFeedMolarity()
	s1 := handStage(cf, 1000, 12, 12, 1.8, 1, 1, 298.15, 2)
	s2 := handStage(s1.cb, s1.qb, 11, 10, 1.8, 1, 1, 318.15, 2)
	if math.Abs(res.Stages[1].Outcome.PermeateFlow-s2.qp) > 1e-9 {
		t.Fatalf("第 2 段应按覆盖温度核算：服务 %g，手算 %g", res.Stages[1].Outcome.PermeateFlow, s2.qp)
	}
	// 第 3 段进料只继承第 2 段浓水的浓度与流量，温度回到阵列共用值。
	s3 := handStage(s2.cb, s2.qb, 10, 8, 1.8, 1, 1, 298.15, 2)
	if math.Abs(res.Stages[2].Outcome.PermeateFlow-s3.qp) > 1e-9 {
		t.Fatalf("覆盖不得向后传播：第 3 段服务 %g，手算 %g", res.Stages[2].Outcome.PermeateFlow, s3.qp)
	}
}

func TestEvaluateArray_FirstStageFeedInvalid(t *testing.T) {
	cfg := threeStageConfig()
	cfg.Feed.Solution.MassConcentration = -1 // 负浓度
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("应作为段失败返回而非 Go error: %v", err)
	}
	if res.Failed == nil || res.Failed.Stage != 1 {
		t.Fatalf("首段进料非法应定位第 1 段，实际 %+v", res.Failed)
	}
	if len(res.Stages) != 0 {
		t.Fatal("第 1 段就失败时不应有任何成功段")
	}
}

func TestEvaluateArray_BrineLinksNextFeed(t *testing.T) {
	// 段间衔接不变量：第 i+1 段进料 == 第 i 段浓水（浓度与流量）。
	cfg := threeStageConfig()
	res, err := Evaluate(cfg)
	if err != nil || res.Failed != nil {
		t.Fatalf("应全部跑通: err=%v failed=%v", err, res.Failed)
	}
	for i := 1; i < len(res.Stages); i++ {
		prev, cur := res.Stages[i-1].Outcome, res.Stages[i]
		if cur.Feed.Molarity != prev.BrineMolarity || cur.Feed.Flow != prev.BrineFlow {
			t.Fatalf("第 %d 段进料未衔接第 %d 段浓水", i+1, i)
		}
	}
}
