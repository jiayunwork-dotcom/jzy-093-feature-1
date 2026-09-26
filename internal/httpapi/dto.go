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

	ArrayStagingFormula    string `json:"array_staging_formula"`
	ArrayGlobalSaltFormula string `json:"array_global_salt_balance_formula"`
	BlendedPermeateFormula string `json:"blended_permeate_formula"`
}

type errorDTO struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ---- 多段串联阵列 ----

// stageSpecDTO 是阵列中一段膜的配置。膜面积、渗透系数、截留率、工作压差
// 必须逐段给全：缺省（零值）不会沿用上一段，而是在该段求值时被拒绝并
// 带段号报错。polarization_factor 缺省按 1（无极化）处理。
type stageSpecDTO struct {
	AreaM2             float64 `json:"area_m2"`
	PermeabilityLMH    float64 `json:"permeability_lmh_per_bar"`
	Rejection          float64 `json:"salt_rejection"`
	Polarization       float64 `json:"polarization_factor,omitempty"`
	AppliedPressureBar float64 `json:"applied_pressure_bar"`
}

// arrayConfigDTO 是一整套多段串联阵列配置。
// stage_count 可省；给了就必须为正且与 stages 长度一致，否则 400。
type arrayConfigDTO struct {
	Name       string          `json:"name,omitempty"`
	StageCount *int            `json:"stage_count,omitempty"`
	Feed       feedSolutionDTO `json:"feed"`
	FeedFlowLh float64         `json:"feed_flow_lh"`
	Stages     []stageSpecDTO  `json:"stages"`
}

// evaluateCaseArrayDTO 是“对已登记工况档套用已登记多段配置”的请求体。
type evaluateCaseArrayDTO struct {
	Array string `json:"array"`
}

// stageInputDTO 回显某一段实际生效的输入（进料已是上一段浓水接力后的工况）。
type stageInputDTO struct {
	FeedMolarityMolar     float64 `json:"feed_molarity_mol_per_l"`
	FeedMassConcentration float64 `json:"feed_mass_concentration_g_per_l,omitempty"`
	FeedFlowLh            float64 `json:"feed_flow_lh"`
	TemperatureK          float64 `json:"temperature_k"`
	VanTHoff              float64 `json:"vanth_hoff_factor"`
	AppliedPressureBar    float64 `json:"applied_pressure_bar"`
	AreaM2                float64 `json:"area_m2"`
	PermeabilityLMH       float64 `json:"permeability_lmh_per_bar"`
	Rejection             float64 `json:"salt_rejection"`
	Polarization          float64 `json:"polarization_factor"` // 生效值（缺省 0 已按 1 处理）
}

// stageResultDTO 是一段跑通后的完整中间结果。
type stageResultDTO struct {
	StageIndex int           `json:"stage_index"` // 段号，从 1 开始
	Input      stageInputDTO `json:"input"`

	PermeateMolarityMolar  float64 `json:"permeate_molarity_mol_per_l"`
	BrineMolarityMolar     float64 `json:"brine_molarity_mol_per_l"`
	BrineMassConcentration float64 `json:"brine_mass_concentration_g_per_l,omitempty"`

	FeedOsmoticPressureBar     float64 `json:"feed_osmotic_pressure_bar"`
	WallOsmoticPressureBar     float64 `json:"wall_osmotic_pressure_bar"`
	PermeateOsmoticPressureBar float64 `json:"permeate_osmotic_pressure_bar"`

	NetDrivingPressureBar float64 `json:"net_driving_pressure_bar"`
	PermeateFlowLh        float64 `json:"permeate_flow_lh"`
	BrineFlowLh           float64 `json:"brine_flow_lh"`
	Recovery              float64 `json:"recovery"`

	Salt saltFlowsDTO `json:"salt_balance"`
}

// stageFeedSnapshotDTO 是失败段开算前的进料条件快照。
type stageFeedSnapshotDTO struct {
	FeedMolarityMolar      float64 `json:"feed_molarity_mol_per_l"`
	FeedFlowLh             float64 `json:"feed_flow_lh"`
	TemperatureK           float64 `json:"temperature_k"`
	VanTHoff               float64 `json:"vanth_hoff_factor"`
	AppliedPressureBar     float64 `json:"applied_pressure_bar"`
	FeedOsmoticPressureBar float64 `json:"feed_osmotic_pressure_bar"`
	WallOsmoticPressureBar float64 `json:"wall_osmotic_pressure_bar,omitempty"`
}

// failureDTO 定位第一段失败：段号、错误码、原因、该段进料条件。
type failureDTO struct {
	StageIndex int                  `json:"stage_index"`
	Code       string               `json:"code"`
	Reason     string               `json:"reason"`
	StageFeed  stageFeedSnapshotDTO `json:"stage_feed"`
}

// globalSaltDTO 是跨全部段的总盐账：阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量。
type globalSaltDTO struct {
	FeedSaltMolFlow     float64 `json:"feed_salt_mol_per_h"`
	PermeateSaltMolFlow float64 `json:"permeate_salt_mol_per_h"`
	BrineSaltMolFlow    float64 `json:"brine_salt_mol_per_h"`
	ResidualMolFlow     float64 `json:"residual_mol_per_h"`

	FeedSaltMassFlow     float64 `json:"feed_salt_mass_flow_gh,omitempty"`
	PermeateSaltMassFlow float64 `json:"permeate_salt_mass_flow_gh,omitempty"`
	BrineSaltMassFlow    float64 `json:"brine_salt_mass_flow_gh,omitempty"`
	ResidualMassFlow     float64 `json:"residual_gh,omitempty"`
}

// arraySummaryDTO 是全套阵列跑通后的汇总。
type arraySummaryDTO struct {
	StageCount        int     `json:"stage_count"`
	TotalPermeateFlow float64 `json:"total_permeate_flow_lh"`
	OverallRecovery   float64 `json:"overall_recovery"`
	FinalBrineFlow    float64 `json:"final_brine_flow_lh"`

	FinalBrineMolarityMolar     float64 `json:"final_brine_molarity_mol_per_l"`
	FinalBrineMassConcentration float64 `json:"final_brine_mass_concentration_g_per_l,omitempty"`

	// 混合产水浓度按各段实际产水量加权，不是算术平均。
	BlendedPermeateMolarityMolar     float64 `json:"blended_permeate_molarity_mol_per_l"`
	BlendedPermeateMassConcentration float64 `json:"blended_permeate_mass_concentration_g_per_l,omitempty"`

	GlobalSalt globalSaltDTO `json:"global_salt_balance"`
}

// arrayResultDTO 是多段阵列核算的完整响应。
// status 为 "ok" 时 summary 非空、failure 为 null；
// status 为 "stage_failed" 时 failure 非空、summary 为 null，
// stages 里保留失败段之前已跑通的全部中间结果。
type arrayResultDTO struct {
	Name      string `json:"name,omitempty"`
	ArrayName string `json:"array_name,omitempty"` // 套用的已登记多段配置（如有）
	CaseName  string `json:"case_name,omitempty"`  // 套用的已登记工况档（如有）

	Status     string           `json:"status"` // ok / stage_failed
	StageCount int              `json:"stage_count"`
	Stages     []stageResultDTO `json:"stages"`
	Failure    *failureDTO      `json:"failure"`
	Summary    *arraySummaryDTO `json:"summary"`
	Units      unitsDTO         `json:"units"`
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
