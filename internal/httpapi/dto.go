// Package httpapi 用标准库 net/http 暴露反渗透核算服务。
// 服务只提供 JSON over HTTP，不做页面。
package httpapi

import "rocalc/internal/osmotic"

// 单位约定（随文档与字段名一并给出，避免量纲混淆）：
//   - 压力（渗透压、工作压差、背压、NDP）：bar
//   - 摩尔浓度：mol/L；质量浓度：g/L；摩尔质量：g/mol
//   - 温度：K；流量：L/h；膜面积：m²；渗透系数 Lp：LMH = L/(m²·h)/bar
type feedSolutionDTO struct {
	Molarity          *float64 `json:"molarity_mol_per_l,omitempty"`
	MassConcentration *float64 `json:"mass_concentration_g_per_l,omitempty"`
	MolarMass         *float64 `json:"molar_mass_g_per_mol,omitempty"`
	TemperatureK      float64  `json:"temperature_k"`
	VanTHoff          float64  `json:"vanth_hoff_factor"`
}

type specDTO struct {
	Name string `json:"name,omitempty"`

	Feed feedSolutionDTO `json:"feed"`

	AppliedPressureBar float64 `json:"applied_pressure_bar"`
	FeedFlowLh         float64 `json:"feed_flow_lh"`
	PermeabilityLMH    float64 `json:"permeability_lmh_per_bar"`
	AreaM2             float64 `json:"area_m2"`
	Rejection          float64 `json:"salt_rejection"`
	Polarization       float64 `json:"polarization_factor,omitempty"`
}

type saltFlowsDTO struct {
	FeedSaltMassFlowGh     float64 `json:"feed_salt_mass_flow_gh"`
	PermeateSaltMassFlowGh float64 `json:"permeate_salt_mass_flow_gh"`
	BrineSaltMassFlowGh    float64 `json:"brine_salt_mass_flow_gh"`
	ResidualGh             float64 `json:"residual_gh"`
}

type outcomeDTO struct {
	Name string `json:"name,omitempty"`

	FeedMolarityMolar      float64 `json:"feed_molarity_mol_per_l"`
	PermeateMolarityMolar  float64 `json:"permeate_molarity_mol_per_l"`
	BrineMolarityMolar     float64 `json:"brine_molarity_mol_per_l"`
	FeedMassConcentration  float64 `json:"feed_mass_concentration_g_per_l,omitempty"`
	BrineMassConcentration float64 `json:"brine_mass_concentration_g_per_l,omitempty"`

	FeedOsmoticPressureBar     float64 `json:"feed_osmotic_pressure_bar"`
	WallOsmoticPressureBar     float64 `json:"wall_osmotic_pressure_bar"`
	PermeateOsmoticPressureBar float64 `json:"permeate_osmotic_pressure_bar"`

	NetDrivingPressureBar float64 `json:"net_driving_pressure_bar"`
	PermeateFlowLh        float64 `json:"permeate_flow_lh"`
	BrineFlowLh           float64 `json:"brine_flow_lh"`
	Recovery              float64 `json:"recovery"`

	Salt saltFlowsDTO `json:"salt_balance"`

	Units unitsDTO `json:"units"`
}

type unitsDTO struct {
	Pressure     string `json:"pressure"`
	Molarity     string `json:"molarity"`
	MassConc     string `json:"mass_concentration"`
	Temperature  string `json:"temperature"`
	Flow         string `json:"flow"`
	Area         string `json:"area"`
	Permeability string `json:"permeability"`
}

func standardUnits() unitsDTO {
	return unitsDTO{
		Pressure:     "bar",
		Molarity:     "mol/L",
		MassConc:     "g/L",
		Temperature:  "K",
		Flow:         "L/h",
		Area:         "m^2",
		Permeability: "LMH = L/(m^2*h)/bar",
	}
}

type constantsDTO struct {
	GasConstant         float64 `json:"gas_constant_r_bar_l_mol_k"`
	OsmoticFormula      string  `json:"osmotic_formula"`
	NDFormula           string  `json:"ndp_formula"`
	PermeateFormula     string  `json:"permeate_flow_formula"`
	BrineLimitFormula   string  `json:"brine_full_rejection_formula"`
	BrinePartialFormula string  `json:"brine_partial_rejection_formula"`
}

type errorDTO struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ---- 多段串联阵列 ----

// arrayStageDTO 是一段膜管的配置。膜参数必须逐段给全（指针区分“没给”与“给了 0”）；
// 极化因子省略按 1（无极化）处理；温度/范特霍夫因子为可选的段级覆盖。
type arrayStageDTO struct {
	AppliedPressureBar *float64 `json:"applied_pressure_bar"`
	PermeabilityLMH    *float64 `json:"permeability_lmh_per_bar"`
	AreaM2             *float64 `json:"area_m2"`
	Rejection          *float64 `json:"salt_rejection"`
	Polarization       float64  `json:"polarization_factor,omitempty"`
	TemperatureK       float64  `json:"temperature_k,omitempty"`
	VanTHoff           float64  `json:"vanth_hoff_factor,omitempty"`
}

// arrayConfigDTO 是登记用的阵列配置：只有膜参数，不含进料工况
// （点名核算时进料由所套用的工况档提供）。
// StageCount 为可选的声明段数：给了就必须 >=1 且与 stages 实际长度一致。
type arrayConfigDTO struct {
	Name       string          `json:"name,omitempty"`
	StageCount *int            `json:"stage_count,omitempty"`
	Stages     []arrayStageDTO `json:"stages"`
}

// arrayAdhocDTO 是临时阵列核算请求：整套配置 + 阵列共用进料工况。
type arrayAdhocDTO struct {
	Name       string          `json:"name,omitempty"`
	StageCount *int            `json:"stage_count,omitempty"`
	Feed       feedSolutionDTO `json:"feed"`
	FeedFlowLh float64         `json:"feed_flow_lh"`
	Stages     []arrayStageDTO `json:"stages"`
}

// arrayCaseEvaluateDTO 是“对已登记工况档套用已登记阵列配置”的请求体。
type arrayCaseEvaluateDTO struct {
	CaseName string `json:"case_name"`
}

type stageFeedDTO struct {
	MolarityMolar float64 `json:"molarity_mol_per_l"`
	FlowLh        float64 `json:"flow_lh"`
	TemperatureK  float64 `json:"temperature_k"`
	VanTHoff      float64 `json:"vanth_hoff_factor"`
}

// stageOutcomeDTO 是某一段的中间结果：进料条件 + 与单段接口同构的核算结果。
type stageOutcomeDTO struct {
	Stage   int          `json:"stage"`
	Feed    stageFeedDTO `json:"feed"`
	Outcome outcomeDTO   `json:"outcome"`
}

type stageErrorDTO struct {
	Stage  int          `json:"stage"`
	Feed   stageFeedDTO `json:"feed"`
	Code   string       `json:"code"`
	Reason string       `json:"reason"`
}

type globalSaltDTO struct {
	FeedSaltMolarFlowMh     float64 `json:"feed_salt_molar_flow_molh"`
	PermeateSaltMolarFlowMh float64 `json:"permeate_salt_molar_flow_molh"`
	BrineSaltMolarFlowMh    float64 `json:"brine_salt_molar_flow_molh"`
	ResidualMolarMh         float64 `json:"residual_molh"`

	// 质量口径：给了摩尔质量时四个字段全出现（合法的 0 也保留，如完全截留时产水零盐）；
	// 没给摩尔质量时四个字段整体缺席，不拿“全 0”冒充已核算。
	FeedSaltMassFlowGh     *float64 `json:"feed_salt_mass_flow_gh,omitempty"`
	PermeateSaltMassFlowGh *float64 `json:"permeate_salt_mass_flow_gh,omitempty"`
	BrineSaltMassFlowGh    *float64 `json:"brine_salt_mass_flow_gh,omitempty"`
	ResidualMassGh         *float64 `json:"residual_gh,omitempty"`
}

type arraySummaryDTO struct {
	TotalPermeateFlowLh         float64       `json:"total_permeate_flow_lh"`
	OverallRecovery             float64       `json:"overall_recovery"`
	FinalBrineFlowLh            float64       `json:"final_brine_flow_lh"`
	FinalBrineMolarityMolar     float64       `json:"final_brine_molarity_mol_per_l"`
	FinalBrineMassConcentration float64       `json:"final_brine_mass_concentration_g_per_l,omitempty"`
	WeightedPermeateMolarity    float64       `json:"weighted_permeate_molarity_mol_per_l"`
	GlobalSalt                  globalSaltDTO `json:"global_salt_balance"`
}

// arrayResultDTO 是阵列核算的统一响应：全部成功 status=ok；
// 某段失败 status=stage_failed，HTTP 400，前面已跑通的段原样保留在 stages 里。
type arrayResultDTO struct {
	Name    string            `json:"name,omitempty"`
	Status  string            `json:"status"`
	Stages  []stageOutcomeDTO `json:"stages"`
	Summary *arraySummaryDTO  `json:"summary,omitempty"`
	Failed  *stageErrorDTO    `json:"failed,omitempty"`
	Code    string            `json:"code,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Units   unitsDTO          `json:"units"`
}

func toSolutionDTO(s osmotic.Solution) feedSolutionDTO {
	dto := feedSolutionDTO{
		TemperatureK: s.Temperature,
		VanTHoff:     s.VanTHoff,
	}
	if s.Molarity != 0 {
		v := s.Molarity
		dto.Molarity = &v
	}
	if s.MassConcentration != 0 {
		v := s.MassConcentration
		dto.MassConcentration = &v
	}
	if s.MolarMass != 0 {
		v := s.MolarMass
		dto.MolarMass = &v
	}
	return dto
}
