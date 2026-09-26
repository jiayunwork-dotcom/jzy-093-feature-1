// Package array 在单段膜核算内核（membrane.Evaluate）之上编排“多段串联阵列”：
// 上一段的浓水直接作为下一段的进料，逐段推进直到末段，
// 返回每一段的中间结果、首个失败段的定位，以及全阵列汇总。
//
// 编排层只做三件事，不重复实现任何膜公式：
//  1. 把第 i 段的浓水（浓度、流量）原地翻成第 i+1 段的进料工况；
//  2. 每段各自走一遍 membrane.Evaluate 的全套合法性与守恒校验；
//  3. 跨全部段独立校验一笔更大的账——全局盐物料守恒
//     （首段进料盐量 = 各段产水盐量之和 + 末段浓水盐量），
//     它与单段内部守恒是两笔账，各自独立校验、互不替代。
//
// 参数规则（对调用方固定、可预期、可验证）：
//   - 膜面积、渗透系数、盐截留率、工作压差：必须逐段给全，
//     不存在“沿用上一段”的隐式继承，缺了按 CodeMissingStageParameter 拒绝；
//   - 浓差极化因子：省略按 1（无极化）处理，与单段内核语义一致；
//   - 温度、范特霍夫因子：阵列共用一份（点名核算时取工况档的），
//     某段显式覆盖时以该段为准，且只影响该段，不向后传播；
//   - 段与段之间只传播浓水的浓度与流量，其余一律不跨段传播。
package array

import (
	"fmt"
	"math"

	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// Feed 是整套阵列共用的进料工况（浓度、温度、范特霍夫因子、进料流量）。
type Feed struct {
	Solution osmotic.Solution // 进料溶液（浓度口径与单段一致）
	Flow     float64          // 首段进料流量 Qf1，L/h
}

// Stage 是一段膜管的配置。膜参数必须逐段给全（见包注释）。
type Stage struct {
	Area         float64 // 膜面积 A，m²
	Permeability float64 // 渗透系数 Lp，LMH/bar
	Rejection    float64 // 盐截留率 r，(0,1]
	Polarization float64 // 浓差极化因子 β；0 按 1（无极化）处理
	Applied      float64 // 该段工作压差 Δp，bar
	TemperatureK float64 // 可选：该段温度覆盖，K；0 表示用阵列共用的
	VanTHoff     float64 // 可选：该段范特霍夫因子覆盖；0 表示用阵列共用的
}

// Config 是一整套多段串联阵列配置。
type Config struct {
	Name   string // 配置名（登记时用；临时核算可空）
	Feed   Feed   // 阵列共用进料工况
	Stages []Stage
}

// StageFeed 是某一段实际使用的进料条件（含覆盖生效后的温度/因子），
// 随每段结果返回，失败段也带，便于调用方核对“这段是在什么条件下算的”。
type StageFeed struct {
	Molarity    float64 // 进料摩尔浓度，mol/L
	Flow        float64 // 进料流量，L/h
	Temperature float64 // 实际采用温度，K
	VanTHoff    float64 // 实际采用范特霍夫因子
}

// StageOutcome 是某一段的中间结果：进料条件 + 单段核算结果。
type StageOutcome struct {
	Stage   int               // 段号，从 1 开始封装时由编排层填
	Feed    StageFeed         // 该段实际进料条件
	Outcome *membrane.Outcome // 单段核算结果（与单段接口同一内核产出）
}

// StageError 定位首个失败段：哪一段、该段进料条件、内核错误码与原因。
type StageError struct {
	Stage  int       // 失败段号（从 1 开始）
	Feed   StageFeed // 失败段的进料条件
	Code   string    // 内核稳定错误码（如 non_positive_ndp）
	Reason string    // 中文原因
}

// GlobalSalt 是跨全部段的全局盐物料守恒账（摩尔口径必校，给了摩尔质量再校质量口径）。
type GlobalSalt struct {
	FeedMolarFlow     float64 // 首段进料盐摩尔流量，mol/h
	PermeateMolarFlow float64 // 各段产水盐摩尔流量之和，mol/h
	BrineMolarFlow    float64 // 末段浓水盐摩尔流量，mol/h
	ResidualMolar     float64 // 摩尔残差 = 进料 −（产水和 + 浓水），mol/h

	FeedMassFlow     float64 // 首段进料盐质量流量，g/h（无摩尔质量时为 0）
	PermeateMassFlow float64 // 各段产水盐质量流量之和，g/h
	BrineMassFlow    float64 // 末段浓水盐质量流量，g/h
	ResidualMass     float64 // 质量残差，g/h
}

// Summary 是整套阵列的汇总。各段产水混合浓度按产水量加权，
// 不是各段产水浓度的算术平均。
type Summary struct {
	TotalPermeateFlow     float64 // 汇总产水量 ΣQp_i，L/h
	OverallRecovery       float64 // 总回收率 ΣQp_i / Qf1
	FinalBrineFlow        float64 // 末段浓水流量，L/h
	FinalBrineMolarity    float64 // 末段浓水浓度，mol/L
	WeightedPermeateMolar float64 // 产水量加权平均浓度 Σ(Cp_i·Qp_i)/ΣQp_i，mol/L
	GlobalSalt            GlobalSalt
}

// Result 是一次阵列核算的完整返回。
// 任何情况下 Stages 都保留已成功段的中间结果：
// 某段失败时 Failed 非空、Summary 为空，但前面跑通的段原样保留。
type Result struct {
	Name    string
	Stages  []StageOutcome // 已成功段（失败时只含失败段之前的段）
	Summary *Summary       // 全部段成功且全局守恒校验通过时非空
	Failed  *StageError    // 首个失败段定位；全部成功为 nil
}

// ValidateConfig 静态校验配置结构：段数合法、每段必填膜参数齐全。
// declared 为可选的“声明段数”：非 nil 时必须 >=1 且与实际段数一致；
// 传 nil 表示未声明。显式给 0 或负数同样拒绝。
// 数值合法性（正压差、(0,1] 截留率等）不做提前判定，留到逐段执行时
// 由内核以失败段定位报出，保证“第几段、什么原因”不被静态检查截胡。
func ValidateConfig(cfg Config, declared *int) error {
	if len(cfg.Stages) == 0 {
		return validation.NewError(validation.CodeInvalidStageCount,
			"段数必须 >= 1，实际为 0")
	}
	if declared != nil {
		if *declared < 1 {
			return validation.NewError(validation.CodeInvalidStageCount,
				fmt.Sprintf("声明段数必须 >= 1，实际为 %d", *declared))
		}
		if *declared != len(cfg.Stages) {
			return validation.NewError(validation.CodeInvalidStageCount,
				fmt.Sprintf("声明段数 %d 与实际段数 %d 不一致", *declared, len(cfg.Stages)))
		}
	}
	for i, st := range cfg.Stages {
		if st.Area == 0 {
			return missingStageParam(i, "膜面积 area_m2")
		}
		if st.Permeability == 0 {
			return missingStageParam(i, "渗透系数 permeability_lmh_per_bar")
		}
		if st.Rejection == 0 {
			return missingStageParam(i, "盐截留率 salt_rejection")
		}
		if st.Applied == 0 {
			return missingStageParam(i, "工作压差 applied_pressure_bar")
		}
	}
	return nil
}

func missingStageParam(i int, field string) error {
	return validation.NewError(validation.CodeMissingStageParameter,
		fmt.Sprintf("第 %d 段缺少必填参数 %s（膜参数必须逐段给全，不沿用上一段）", i+1, field))
}

// Evaluate 按段推进执行整套阵列核算。
//
// 返回约定：配置结构非法（段数、缺参）时 err 为 *validation.DomainError 且 res 为 nil；
// 执行中某段被判非法工况时 err 为 nil，res.Failed 携带失败段定位，
// res.Stages 原样保留失败段之前已跑通的全部中间结果。
func Evaluate(cfg Config) (*Result, error) {
	if err := ValidateConfig(cfg, nil); err != nil {
		return nil, err
	}

	res := &Result{Name: cfg.Name, Stages: make([]StageOutcome, 0, len(cfg.Stages))}

	// 首段进料：阵列共用工况。
	cf, err := cfg.Feed.Solution.EffectiveMolarity()
	if err != nil {
		// 首段进料本身不合法：定位为第 1 段失败（浓度不可解析时留 0）。
		res.Failed = stageError(1, cfg, 0, cfg.Feed.Flow, err)
		return res, nil
	}
	stageMolarity := cf
	stageFlow := cfg.Feed.Flow

	for i, st := range cfg.Stages {
		temp := cfg.Feed.Solution.Temperature
		if st.TemperatureK != 0 {
			temp = st.TemperatureK // 段级覆盖优先，仅本段生效
		}
		vanthoff := cfg.Feed.Solution.VanTHoff
		if st.VanTHoff != 0 {
			vanthoff = st.VanTHoff
		}

		sol := osmotic.Solution{
			Molarity:    stageMolarity,
			Temperature: temp,
			VanTHoff:    vanthoff,
		}
		if m := cfg.Feed.Solution.MolarMass; m > 0 {
			// 摩尔质量随段传递以启用质量口径盐账；同时给出自洽的质量浓度，
			// 否则“只给 Molarity + MolarMass”会被内核判为浓度口径不自洽。
			sol.MolarMass = m
			sol.MassConcentration = stageMolarity * m
		}
		spec := membrane.FeedSpec{
			Feed:            sol,
			AppliedPressure: st.Applied,
			FeedFlow:        stageFlow,
			Permeability:    st.Permeability,
			Area:            st.Area,
			Rejection:       st.Rejection,
			Polarization:    st.Polarization,
		}

		out, err := membrane.Evaluate(spec)
		if err != nil {
			res.Failed = stageError(i+1, cfg, stageMolarity, stageFlow, err)
			return res, nil
		}

		res.Stages = append(res.Stages, StageOutcome{
			Stage: i + 1,
			Feed: StageFeed{
				Molarity:    stageMolarity,
				Flow:        stageFlow,
				Temperature: temp,
				VanTHoff:    vanthoff,
			},
			Outcome: out,
		})

		// 段间衔接：本段浓水的浓度与流量原地翻成下一段进料。
		stageMolarity = out.BrineMolarity
		stageFlow = out.BrineFlow
	}

	summary, err := summarize(cfg, res.Stages)
	if err != nil {
		// 全局守恒账不闭合：中间结果原样保留，只标失败。
		res.Failed = &StageError{
			Stage:  len(cfg.Stages),
			Code:   validation.CodeArraySaltBalanceViolation,
			Reason: err.Error(),
		}
		return res, nil
	}
	res.Summary = summary
	return res, nil
}

// stageError 把内核错误翻成带段号与进料条件的定位信息。
func stageError(stage int, cfg Config, molarity, flow float64, err error) *StageError {
	se := &StageError{Stage: stage, Reason: err.Error()}
	if stage >= 1 && stage <= len(cfg.Stages) {
		st := cfg.Stages[stage-1]
		temp := cfg.Feed.Solution.Temperature
		if st.TemperatureK != 0 {
			temp = st.TemperatureK
		}
		vanthoff := cfg.Feed.Solution.VanTHoff
		if st.VanTHoff != 0 {
			vanthoff = st.VanTHoff
		}
		se.Feed = StageFeed{Molarity: molarity, Flow: flow, Temperature: temp, VanTHoff: vanthoff}
	}
	if de, ok := err.(*validation.DomainError); ok {
		se.Code = de.Code
		se.Reason = de.Reason
	} else {
		se.Code = "internal_error"
	}
	return se
}

// 全局守恒容差与单段口径一致：相对 1e-9，近零时绝对下限兜底。
const (
	arrayBalanceRelTol = 1e-9
	arrayBalanceFloor  = 1e-12
)

// summarize 汇总整套阵列并独立校验全局盐物料守恒：
//
//	首段进料盐量 = Σ各段产水盐量 + 末段浓水盐量
//
// 这笔账跨所有段，与每段内部的守恒各自独立校验，互不替代。
func summarize(cfg Config, stages []StageOutcome) (*Summary, error) {
	s := &Summary{}
	var permeateMol float64
	for _, st := range stages {
		o := st.Outcome
		s.TotalPermeateFlow += o.PermeateFlow
		permeateMol += o.PermeateMolarity * o.PermeateFlow
	}
	last := stages[len(stages)-1].Outcome
	s.OverallRecovery = s.TotalPermeateFlow / cfg.Feed.Flow
	s.FinalBrineFlow = last.BrineFlow
	s.FinalBrineMolarity = last.BrineMolarity
	if s.TotalPermeateFlow > 0 {
		s.WeightedPermeateMolar = permeateMol / s.TotalPermeateFlow
	}

	// 全局盐账（摩尔口径必校）。
	feedMol := stages[0].Feed.Molarity * cfg.Feed.Flow
	brineMol := last.BrineMolarity * last.BrineFlow
	s.GlobalSalt.FeedMolarFlow = feedMol
	s.GlobalSalt.PermeateMolarFlow = permeateMol
	s.GlobalSalt.BrineMolarFlow = brineMol
	s.GlobalSalt.ResidualMolar = feedMol - permeateMol - brineMol

	tol := arrayBalanceRelTol * math.Max(math.Abs(feedMol), math.Max(math.Abs(permeateMol), math.Abs(brineMol)))
	if tol < arrayBalanceFloor {
		tol = arrayBalanceFloor
	}
	if math.Abs(s.GlobalSalt.ResidualMolar) > tol {
		return nil, fmt.Errorf("全局盐量不守恒：首段进料 %.9g mol/h ≠ 各段产水和 %.9g + 末段浓水 %.9g（残差 %.3g mol/h）",
			feedMol, permeateMol, brineMol, s.GlobalSalt.ResidualMolar)
	}

	// 给了摩尔质量再校质量口径。
	if m := cfg.Feed.Solution.MolarMass; m > 0 {
		s.GlobalSalt.FeedMassFlow = feedMol * m
		s.GlobalSalt.PermeateMassFlow = permeateMol * m
		s.GlobalSalt.BrineMassFlow = brineMol * m
		s.GlobalSalt.ResidualMass = s.GlobalSalt.FeedMassFlow - s.GlobalSalt.PermeateMassFlow - s.GlobalSalt.BrineMassFlow
		tolM := arrayBalanceRelTol * math.Max(math.Abs(s.GlobalSalt.FeedMassFlow),
			math.Max(math.Abs(s.GlobalSalt.PermeateMassFlow), math.Abs(s.GlobalSalt.BrineMassFlow)))
		if tolM < arrayBalanceFloor {
			tolM = arrayBalanceFloor
		}
		if math.Abs(s.GlobalSalt.ResidualMass) > tolM {
			return nil, fmt.Errorf("全局盐量不守恒（质量口径）：首段进料 %.9g g/h ≠ 各段产水和 %.9g + 末段浓水 %.9g（残差 %.3g g/h）",
				s.GlobalSalt.FeedMassFlow, s.GlobalSalt.PermeateMassFlow, s.GlobalSalt.BrineMassFlow, s.GlobalSalt.ResidualMass)
		}
	}
	return s, nil
}
