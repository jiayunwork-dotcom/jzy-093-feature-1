package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"rocalc/internal/array"
	"rocalc/internal/cases"
	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// 多段串联阵列的 HTTP 端点：
//
//	GET    /v1/arrays                     列出已登记阵列配置
//	POST   /v1/arrays                     登记阵列配置（重名 409）
//	PUT    /v1/arrays/{name}              登记或整体替换
//	GET    /v1/arrays/{name}              取回配置
//	DELETE /v1/arrays/{name}              删除
//	POST   /v1/arrays/{name}/evaluate     对已登记工况档套用该配置核算（body: {"case_name": "..."}）
//	POST   /v1/arrays/evaluate            临时提交整套配置 + 共用进料工况直接核算
//
// 响应约定：全部段成功 → 200 + status=ok + summary；
// 某段被判非法工况 → 400 + status=stage_failed + failed（段号/进料条件/原因），
// 失败段之前已跑通的中间结果原样保留在 stages 里，绝不清空。

func (s *Server) handleListArrays(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"arrays": s.arrays.List()})
}

func (s *Server) handleCreateArray(w http.ResponseWriter, req *http.Request) {
	dto, ok := decodeArrayConfig(w, req)
	if !ok {
		return
	}
	if dto.Name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   validation.CodeNonPositiveParameter,
			Reason: "登记阵列配置必须提供 name",
		})
		return
	}
	spec, err := arraySpecFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if err := s.arrays.Register(spec); err != nil {
		if isErr(err, cases.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, &validation.DomainError{
				Code:   "array_already_exists",
				Reason: err.Error(),
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/arrays/"+spec.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"name": spec.Name, "status": "registered"})
}

func (s *Server) handleUpsertArray(w http.ResponseWriter, req *http.Request) {
	dto, ok := decodeArrayConfig(w, req)
	if !ok {
		return
	}
	name := req.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code: validation.CodeNonPositiveParameter, Reason: "路径缺少阵列配置名称",
		})
		return
	}
	dto.Name = name
	spec, err := arraySpecFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	s.arrays.Put(spec)
	writeJSON(w, http.StatusOK, map[string]any{"name": spec.Name, "status": "stored"})
}

func (s *Server) handleGetArray(w http.ResponseWriter, req *http.Request) {
	spec, err := s.arrays.Get(req.PathValue("name"))
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, arraySpecToDTO(spec))
}

func (s *Server) handleDeleteArray(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if err := s.arrays.Delete(name); err != nil {
		writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "deleted"})
}

// handleEvaluateArray 对已登记工况档套用已登记阵列配置：
// 进料工况（浓度、温度、范特霍夫因子、进料流量）取自工况档，
// 膜参数取自阵列配置；段级显式覆盖优先于工况档的共用值。
func (s *Server) handleEvaluateArray(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	spec, err := s.arrays.Get(name)
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	defer req.Body.Close()
	var body arrayCaseEvaluateDTO
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   "malformed_json",
			Reason: "请求体不是合法 JSON 或含未知字段: " + err.Error(),
		})
		return
	}
	if body.CaseName == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   validation.CodeNonPositiveParameter,
			Reason: "必须提供 case_name：阵列核算的进料工况取自该工况档",
		})
		return
	}
	feedCase, err := s.registry.Get(body.CaseName)
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "case_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}

	cfg := spec.Config
	cfg.Name = spec.Name
	cfg.Feed = array.Feed{Solution: feedCase.Feed, Flow: feedCase.FeedFlow}
	s.runArray(w, cfg)
}

// handleEvaluateArrayAdhoc 临时提交整套阵列配置与共用进料工况直接核算。
func (s *Server) handleEvaluateArrayAdhoc(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	var dto arrayAdhocDTO
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   "malformed_json",
			Reason: "请求体不是合法 JSON 或含未知字段: " + err.Error(),
		})
		return
	}
	stages, err := stagesFromDTO(dto.Stages)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	cfg := array.Config{
		Name: dto.Name,
		Feed: array.Feed{
			Solution: solutionFromDTO(dto.Feed),
			Flow:     dto.FeedFlowLh,
		},
		Stages: stages,
	}
	if err := array.ValidateConfig(cfg, dto.StageCount); err != nil {
		writeDomainError(w, err)
		return
	}
	s.runArray(w, cfg)
}

// runArray 执行阵列核算并写出统一响应：
// 全部成功 200；某段非法 400 且保留已成功段的中间结果。
func (s *Server) runArray(w http.ResponseWriter, cfg array.Config) {
	res, err := array.Evaluate(cfg)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	dto := arrayResultToDTO(res, cfg.Feed.Solution.MolarMass)
	if res.Failed != nil {
		writeJSON(w, http.StatusBadRequest, dto)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ---- 编解码 ----

func decodeArrayConfig(w http.ResponseWriter, req *http.Request) (arrayConfigDTO, bool) {
	defer req.Body.Close()
	var dto arrayConfigDTO
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   "malformed_json",
			Reason: "请求体不是合法 JSON 或含未知字段: " + err.Error(),
		})
		return dto, false
	}
	return dto, true
}

func solutionFromDTO(dto feedSolutionDTO) osmotic.Solution {
	sol := osmotic.Solution{
		Temperature: dto.TemperatureK,
		VanTHoff:    dto.VanTHoff,
	}
	if dto.Molarity != nil {
		sol.Molarity = *dto.Molarity
	}
	if dto.MassConcentration != nil {
		sol.MassConcentration = *dto.MassConcentration
	}
	if dto.MolarMass != nil {
		sol.MolarMass = *dto.MolarMass
	}
	return sol
}

// stagesFromDTO 把请求里的段配置翻成内核结构；
// 必填膜参数（压差、渗透系数、膜面积、截留率）缺一个就拒绝并指出段号。
func stagesFromDTO(dtos []arrayStageDTO) ([]array.Stage, error) {
	stages := make([]array.Stage, len(dtos))
	for i, d := range dtos {
		st := array.Stage{
			Polarization: d.Polarization,
			TemperatureK: d.TemperatureK,
			VanTHoff:     d.VanTHoff,
		}
		if d.AppliedPressureBar == nil {
			return nil, missingStageField(i, "applied_pressure_bar")
		}
		st.Applied = *d.AppliedPressureBar
		if d.PermeabilityLMH == nil {
			return nil, missingStageField(i, "permeability_lmh_per_bar")
		}
		st.Permeability = *d.PermeabilityLMH
		if d.AreaM2 == nil {
			return nil, missingStageField(i, "area_m2")
		}
		st.Area = *d.AreaM2
		if d.Rejection == nil {
			return nil, missingStageField(i, "salt_rejection")
		}
		st.Rejection = *d.Rejection
		stages[i] = st
	}
	return stages, nil
}

func missingStageField(i int, field string) error {
	return validation.NewError(validation.CodeMissingStageParameter,
		"第 "+strconv.Itoa(i+1)+" 段缺少必填参数 "+field+"（膜参数必须逐段给全，不沿用上一段）")
}

func arraySpecFromDTO(dto arrayConfigDTO) (cases.ArraySpec, error) {
	stages, err := stagesFromDTO(dto.Stages)
	if err != nil {
		return cases.ArraySpec{}, err
	}
	spec := cases.ArraySpec{
		Name: dto.Name,
		Config: array.Config{
			Name:   dto.Name,
			Stages: stages,
		},
	}
	if err := array.ValidateConfig(spec.Config, dto.StageCount); err != nil {
		return cases.ArraySpec{}, err
	}
	return spec, nil
}

func arraySpecToDTO(spec cases.ArraySpec) arrayConfigDTO {
	dto := arrayConfigDTO{
		Name:       spec.Name,
		StageCount: ptr(len(spec.Stages)),
		Stages:     make([]arrayStageDTO, len(spec.Stages)),
	}
	for i, st := range spec.Stages {
		applied, lp, area, rej := st.Applied, st.Permeability, st.Area, st.Rejection
		dto.Stages[i] = arrayStageDTO{
			AppliedPressureBar: &applied,
			PermeabilityLMH:    &lp,
			AreaM2:             &area,
			Rejection:          &rej,
			Polarization:       st.Polarization,
			TemperatureK:       st.TemperatureK,
			VanTHoff:           st.VanTHoff,
		}
	}
	return dto
}

func ptr[T any](v T) *T { return &v }

func stageFeedToDTO(f array.StageFeed) stageFeedDTO {
	return stageFeedDTO{
		MolarityMolar: f.Molarity,
		FlowLh:        f.Flow,
		TemperatureK:  f.Temperature,
		VanTHoff:      f.VanTHoff,
	}
}

func arrayResultToDTO(res *array.Result, molarMass float64) arrayResultDTO {
	dto := arrayResultDTO{
		Name:   res.Name,
		Stages: make([]stageOutcomeDTO, len(res.Stages)),
		Units:  standardUnits(),
	}
	for i, st := range res.Stages {
		// 单段结果复用单段接口的 DTO 结构，字段口径完全一致。
		spec := membrane.FeedSpec{Feed: osmotic.Solution{MolarMass: molarMass}}
		dto.Stages[i] = stageOutcomeDTO{
			Stage:   st.Stage,
			Feed:    stageFeedToDTO(st.Feed),
			Outcome: outcomeToDTO("", spec, st.Outcome),
		}
	}
	if res.Summary != nil {
		s := res.Summary
		dto.Status = "ok"
		dto.Summary = &arraySummaryDTO{
			TotalPermeateFlowLh:      s.TotalPermeateFlow,
			OverallRecovery:          s.OverallRecovery,
			FinalBrineFlowLh:         s.FinalBrineFlow,
			FinalBrineMolarityMolar:  s.FinalBrineMolarity,
			WeightedPermeateMolarity: s.WeightedPermeateMolar,
			GlobalSalt: globalSaltDTO{
				FeedSaltMolarFlowMh:     s.GlobalSalt.FeedMolarFlow,
				PermeateSaltMolarFlowMh: s.GlobalSalt.PermeateMolarFlow,
				BrineSaltMolarFlowMh:    s.GlobalSalt.BrineMolarFlow,
				ResidualMolarMh:         s.GlobalSalt.ResidualMolar,
			},
		}
		if molarMass > 0 {
			dto.Summary.FinalBrineMassConcentration = s.FinalBrineMolarity * molarMass
			g := &dto.Summary.GlobalSalt
			g.FeedSaltMassFlowGh = ptr(s.GlobalSalt.FeedMassFlow)
			g.PermeateSaltMassFlowGh = ptr(s.GlobalSalt.PermeateMassFlow)
			g.BrineSaltMassFlowGh = ptr(s.GlobalSalt.BrineMassFlow)
			g.ResidualMassGh = ptr(s.GlobalSalt.ResidualMass)
		}
	}
	if res.Failed != nil {
		dto.Status = "stage_failed"
		dto.Failed = &stageErrorDTO{
			Stage:  res.Failed.Stage,
			Feed:   stageFeedToDTO(res.Failed.Feed),
			Code:   res.Failed.Code,
			Reason: res.Failed.Reason,
		}
		dto.Code = res.Failed.Code
		dto.Reason = res.Failed.Reason
	}
	return dto
}
