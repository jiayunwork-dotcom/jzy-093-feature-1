package array

import (
	"errors"
	"fmt"
	"math"

	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// 全局衡算残差容差，与 membrane 包单段衡算同一口径：
// 相对 1e−9，盐量接近 0 时用绝对下限兜底。
const (
	globalBalanceRelTol     = 1e-9
	globalBalanceMolarFloor = 1e-12 // mol/h
	globalBalanceMassFloor  = 1e-12 // g/h
)

// ValidateConfig 做阵列级（结构性）校验：段列表非空、阵列进料工况合法。
//
// 不做逐段参数与物理校验——那些在 Evaluate 推进到对应段时才进行，
// 失败会带段号进入 Result.Failure，而不是让整份配置失去登记资格
// （否则“中段压差不足”这类物理非法的配置根本无法登记与演示）。
func ValidateConfig(cfg Config) error {
	if len(cfg.Stages) == 0 {
		return validation.NewError(validation.CodeInvalidStageCount,
			"段列表为空：多段阵列至少需要 1 段（段数必须为正）")
	}
	if err := validation.Positive(cfg.FeedFlow, "阵列进料流量"); err != nil {
		return err
	}
	if err := validation.Temperature(cfg.Feed.Temperature); err != nil {
		return err
	}
	if err := validation.VanTHoff(cfg.Feed.VanTHoff); err != nil {
		return err
	}
	if _, err := cfg.Feed.EffectiveMolarity(); err != nil {
		return err
	}
	return nil
}

// Evaluate 按段推进整套阵列。
//
// 返回的 error 只表示阵列级问题（配置结构性非法，或全局盐守恒被破坏）；
// 某一段的非法工况不作为 error 抛出，而是记入 Result.Failure：
// 前面已跑通段的结果原样保留在 Result.Stages 里，推进在失败段停止。
func Evaluate(cfg Config) (*Result, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	res := &Result{Status: StatusOK, StageCount: len(cfg.Stages), Stages: []StageResult{}}

	// 第 1 段：阵列级进料原样进入，与单段接口拿到的是同一份工况，
	// 因此段数为 1 时结果与 membrane.Evaluate 逐位一致。
	feed := cfg.Feed
	feedFlow := cfg.FeedFlow

	for i, st := range cfg.Stages {
		spec := membrane.FeedSpec{
			Feed:            feed,
			AppliedPressure: st.AppliedPressure,
			FeedFlow:        feedFlow,
			Permeability:    st.Permeability,
			Area:            st.Area,
			Rejection:       st.Rejection,
			Polarization:    st.Polarization,
		}
		out, err := membrane.Evaluate(spec)
		if err != nil {
			res.Status = StatusStageFailed
			res.Failure = &Failure{
				StageIndex: i + 1,
				Code:       failureCode(err),
				Reason:     failureReason(err),
				Feed:       snapshotStageFeed(spec),
			}
			return res, nil
		}
		res.Stages = append(res.Stages, StageResult{Index: i + 1, Spec: spec, Out: *out})

		// 段间接力：上一段的浓水就是下一段的进料。
		// 浓度、流量逐项传递；温度、范特霍夫因子、摩尔质量随阵列级工况不变。
		feed = brineAsFeed(out, cfg.Feed)
		feedFlow = out.BrineFlow
	}

	sum, err := summarize(cfg, res.Stages)
	if err != nil {
		return nil, err
	}
	res.Summary = sum
	return res, nil
}

// brineAsFeed 把一段的浓水翻成下一段的进料溶液。
// 摩尔浓度取浓水浓度；给了摩尔质量时同时带上自洽的质量浓度，
// 使下一段内核照常输出质量口径的盐流量。
func brineAsFeed(out *membrane.Outcome, base osmotic.Solution) osmotic.Solution {
	f := osmotic.Solution{
		Molarity:    out.BrineMolarity,
		Temperature: base.Temperature,
		VanTHoff:    base.VanTHoff,
	}
	if base.MolarMass > 0 {
		f.MolarMass = base.MolarMass
		f.MassConcentration = out.BrineMolarity * base.MolarMass
	}
	return f
}

// snapshotStageFeed 记录某段开算前的进料条件（尽力而为：
// 段失败时进料本身一定合法——第 1 段已做阵列级校验，后续段是
// 内核算出的浓水——因此渗透压总能算出；异常时留零值）。
func snapshotStageFeed(spec membrane.FeedSpec) StageFeed {
	snap := StageFeed{
		Flow:            spec.FeedFlow,
		Temperature:     spec.Feed.Temperature,
		VanTHoff:        spec.Feed.VanTHoff,
		AppliedPressure: spec.AppliedPressure,
	}
	if cf, err := spec.Feed.EffectiveMolarity(); err == nil {
		snap.Molarity = cf
	}
	if pi, err := osmotic.Pressure(spec.Feed); err == nil {
		snap.OsmoticPressure = pi
		beta := spec.Polarization
		if beta == 0 {
			beta = 1
		}
		if beta >= 1 {
			snap.WallOsmoticPressure = beta * pi
		}
	}
	return snap
}

func failureCode(err error) string {
	var de *validation.DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return "stage_evaluation_error"
}

func failureReason(err error) string {
	var de *validation.DomainError
	if errors.As(err, &de) && de.Reason != "" {
		return de.Reason
	}
	return err.Error()
}

// summarize 汇总全套阵列结果，并独立校验全局盐守恒。
func summarize(cfg Config, stages []StageResult) (*Summary, error) {
	var totalQp, permMol float64
	for _, s := range stages {
		totalQp += s.Out.PermeateFlow
		permMol += s.Out.PermeateMolarity * s.Out.PermeateFlow
	}
	last := stages[len(stages)-1].Out
	feedMol := stages[0].Out.FeedMolarity * cfg.FeedFlow
	brineMol := last.BrineMolarity * last.BrineFlow

	sum := &Summary{
		StageCount:              len(stages),
		TotalPermeateFlow:       totalQp,
		OverallRecovery:         totalQp / cfg.FeedFlow,
		FinalBrineFlow:          last.BrineFlow,
		FinalBrineMolarity:      last.BrineMolarity,
		BlendedPermeateMolarity: permMol / totalQp,
		Salt: GlobalSaltBalance{
			FeedSaltMolFlow:     feedMol,
			PermeateSaltMolFlow: permMol,
			BrineSaltMolFlow:    brineMol,
			ResidualMolFlow:     feedMol - permMol - brineMol,
		},
	}
	if m := cfg.Feed.MolarMass; m > 0 {
		sum.Salt.FeedSaltMassFlow = feedMol * m
		sum.Salt.PermeateSaltMassFlow = permMol * m
		sum.Salt.BrineSaltMassFlow = brineMol * m
		sum.Salt.ResidualMassFlow = sum.Salt.FeedSaltMassFlow -
			sum.Salt.PermeateSaltMassFlow - sum.Salt.BrineSaltMassFlow
	}

	if err := checkGlobalBalance(sum, cfg.Feed.MolarMass); err != nil {
		return nil, err
	}
	return sum, nil
}

// checkGlobalBalance 独立校验跨全部段的盐量守恒：
//
//	阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量
//
// 这笔总账不因为各单段守恒已通过就默认成立，单独复算、单独报告。
// 摩尔口径必校；给了摩尔质量时再校质量口径。
func checkGlobalBalance(sum *Summary, molarMass float64) error {
	b := sum.Salt
	tol := globalBalanceRelTol * math.Max(math.Abs(b.FeedSaltMolFlow),
		math.Max(math.Abs(b.PermeateSaltMolFlow), math.Abs(b.BrineSaltMolFlow)))
	if tol < globalBalanceMolarFloor {
		tol = globalBalanceMolarFloor
	}
	if math.Abs(b.ResidualMolFlow) > tol {
		return validation.NewError(validation.CodeSaltBalanceViolation,
			fmt.Sprintf("全局盐量不守恒：阵列进料 %.9g mol/h ≠ 各段产水之和 %.9g + 末段浓水 %.9g（残差 %.3g mol/h）",
				b.FeedSaltMolFlow, b.PermeateSaltMolFlow, b.BrineSaltMolFlow, b.ResidualMolFlow))
	}

	if molarMass > 0 {
		tolMass := globalBalanceRelTol * math.Max(math.Abs(b.FeedSaltMassFlow),
			math.Max(math.Abs(b.PermeateSaltMassFlow), math.Abs(b.BrineSaltMassFlow)))
		if tolMass < globalBalanceMassFloor {
			tolMass = globalBalanceMassFloor
		}
		if math.Abs(b.ResidualMassFlow) > tolMass {
			return validation.NewError(validation.CodeSaltBalanceViolation,
				fmt.Sprintf("全局盐量不守恒（质量口径）：阵列进料 %.9g g/h ≠ 各段产水之和 %.9g + 末段浓水 %.9g（残差 %.3g g/h）",
					b.FeedSaltMassFlow, b.PermeateSaltMassFlow, b.BrineSaltMassFlow, b.ResidualMassFlow))
		}
	}
	return nil
}
