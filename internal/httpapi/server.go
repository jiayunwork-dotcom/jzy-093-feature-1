package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"rocalc/internal/array"
	"rocalc/internal/cases"
	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
	"rocalc/internal/validation"
)

// Server 装配登记表与 HTTP 路由。
type Server struct {
	registry *cases.Registry
	arrays   *array.Registry
	mux      *http.ServeMux
}

// NewServer 创建服务并注册内置工况档与内置三段串联配置。
func NewServer() *Server {
	return NewServerWithRegistries(cases.NewDefaultRegistry(), array.NewDefaultRegistry())
}

// NewServerWithRegistry 使用指定工况档登记表创建服务（测试可注入空表）。
func NewServerWithRegistry(r *cases.Registry) *Server {
	return NewServerWithRegistries(r, array.NewRegistry())
}

// NewServerWithRegistries 使用指定的工况档与多段配置登记表创建服务。
func NewServerWithRegistries(r *cases.Registry, ar *array.Registry) *Server {
	s := &Server{registry: r, arrays: ar, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 暴露根 handler，便于外层套中间件或挂到自定义端口。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/constants", s.handleConstants)

	s.mux.HandleFunc("GET /v1/cases", s.handleListCases)
	s.mux.HandleFunc("POST /v1/cases", s.handleCreateCase)
	s.mux.HandleFunc("PUT /v1/cases/{name}", s.handleUpsertCase)
	s.mux.HandleFunc("GET /v1/cases/{name}", s.handleGetCase)
	s.mux.HandleFunc("DELETE /v1/cases/{name}", s.handleDeleteCase)
	s.mux.HandleFunc("POST /v1/cases/{name}/evaluate", s.handleEvaluateCase)
	s.mux.HandleFunc("POST /v1/cases/{name}/evaluate-array", s.handleEvaluateCaseArray)

	s.mux.HandleFunc("POST /v1/evaluate", s.handleEvaluateAdhoc)
	s.mux.HandleFunc("POST /v1/evaluate-array", s.handleEvaluateArrayAdhoc)

	s.mux.HandleFunc("GET /v1/arrays", s.handleListArrays)
	s.mux.HandleFunc("POST /v1/arrays", s.handleCreateArray)
	s.mux.HandleFunc("PUT /v1/arrays/{name}", s.handleUpsertArray)
	s.mux.HandleFunc("GET /v1/arrays/{name}", s.handleGetArray)
	s.mux.HandleFunc("DELETE /v1/arrays/{name}", s.handleDeleteArray)
	s.mux.HandleFunc("POST /v1/arrays/{name}/evaluate", s.handleEvaluateArray)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleConstants(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, constantsDTO{
		GasConstant:         osmotic.R,
		OsmoticFormula:      "pi = i*C*R*T  (bar; C in mol/L, T in K)",
		NDFormula:           "NDP = applied_pressure - beta*pi_feed + pi_permeate  (bar)",
		PermeateFormula:     "Qp = area_m2 * Lp_lmh_per_bar * NDP  (L/h)",
		BrineLimitFormula:   "Cb = Cf/(1-Y)   when salt rejection r = 1",
		BrinePartialFormula: "Cb = Cf*(1-(1-r)*Y)/(1-Y); salt: Cf*Qf = Cp*Qp + Cb*Qb",

		ArrayStagingFormula:    "stage k feed = stage k-1 brine (molarity & flow carried over; T, i, molar mass from array-level feed)",
		ArrayGlobalSaltFormula: "feed_salt = sum(stage permeate salt) + final brine salt  (mol/h; g/h when molar mass given)",
		BlendedPermeateFormula: "Cp_blend = sum(Cp_k*Qp_k) / sum(Qp_k)  (flow-weighted, NOT arithmetic mean)",
	})
}

func (s *Server) handleListCases(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"cases": s.registry.List()})
}

func (s *Server) handleCreateCase(w http.ResponseWriter, req *http.Request) {
	dto, ok := decodeSpec(w, req)
	if !ok {
		return
	}
	if dto.Name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   validation.CodeNonPositiveParameter,
			Reason: "登记工况档必须提供 name",
		})
		return
	}
	spec, err := specFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if err := s.registry.Register(spec); err != nil {
		if isErr(err, cases.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, &validation.DomainError{
				Code:   "case_already_exists",
				Reason: err.Error(),
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/cases/"+spec.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"name": spec.Name, "status": "registered"})
}

func (s *Server) handleUpsertCase(w http.ResponseWriter, req *http.Request) {
	dto, ok := decodeSpec(w, req)
	if !ok {
		return
	}
	name := req.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code: validation.CodeNonPositiveParameter, Reason: "路径缺少工况档名称",
		})
		return
	}
	dto.Name = name
	spec, err := specFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	s.registry.Put(spec)
	writeJSON(w, http.StatusOK, map[string]any{"name": spec.Name, "status": "stored"})
}

func (s *Server) handleGetCase(w http.ResponseWriter, req *http.Request) {
	spec, err := s.registry.Get(req.PathValue("name"))
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "case_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	dto := specToDTO(spec)
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleDeleteCase(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if err := s.registry.Delete(name); err != nil {
		writeError(w, http.StatusNotFound, &validation.DomainError{Code: "case_not_found", Reason: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "deleted"})
}

func (s *Server) handleEvaluateCase(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	out, err := s.registry.Evaluate(name)
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "case_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	stored, _ := s.registry.Get(name)
	writeJSON(w, http.StatusOK, outcomeToDTO(name, stored.FeedSpec, out))
}

func (s *Server) handleEvaluateAdhoc(w http.ResponseWriter, req *http.Request) {
	dto, ok := decodeSpec(w, req)
	if !ok {
		return
	}
	spec, err := specFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out, err := membrane.Evaluate(spec.FeedSpec)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outcomeToDTO(spec.Name, spec.FeedSpec, out))
}

// ---- 多段串联阵列 ----

func (s *Server) handleListArrays(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"arrays": s.arrays.List()})
}

func (s *Server) handleCreateArray(w http.ResponseWriter, req *http.Request) {
	var dto arrayConfigDTO
	if !decodeBody(w, req, &dto) {
		return
	}
	if dto.Name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   validation.CodeNonPositiveParameter,
			Reason: "登记多段配置必须提供 name",
		})
		return
	}
	cfg, err := arrayConfigFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if err := array.ValidateConfig(cfg); err != nil {
		writeDomainError(w, err)
		return
	}
	if err := s.arrays.Register(cfg); err != nil {
		if isErr(err, array.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, &validation.DomainError{
				Code:   "array_already_exists",
				Reason: err.Error(),
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/arrays/"+cfg.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"name": cfg.Name, "status": "registered"})
}

func (s *Server) handleUpsertArray(w http.ResponseWriter, req *http.Request) {
	var dto arrayConfigDTO
	if !decodeBody(w, req, &dto) {
		return
	}
	name := req.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code: validation.CodeNonPositiveParameter, Reason: "路径缺少多段配置名称",
		})
		return
	}
	dto.Name = name
	cfg, err := arrayConfigFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if err := array.ValidateConfig(cfg); err != nil {
		writeDomainError(w, err)
		return
	}
	s.arrays.Put(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"name": cfg.Name, "status": "stored"})
}

func (s *Server) handleGetArray(w http.ResponseWriter, req *http.Request) {
	cfg, err := s.arrays.Get(req.PathValue("name"))
	if err != nil {
		if isErr(err, array.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, arrayConfigToDTO(cfg))
}

func (s *Server) handleDeleteArray(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if err := s.arrays.Delete(name); err != nil {
		writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "deleted"})
}

// handleEvaluateArray 点名一份已登记多段配置做整套阵列核算。
// 某段非法不映射 4xx：返回 200 + status=stage_failed，
// 失败段之前的结果原样保留在 stages 里，failure 给出段号与原因。
func (s *Server) handleEvaluateArray(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	cfg, err := s.arrays.Get(name)
	if err != nil {
		if isErr(err, array.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	res, err := array.Evaluate(cfg)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, arrayResultToDTO(res, cfg.Name, name, "", cfg.Feed.MolarMass))
}

// handleEvaluateArrayAdhoc 临时提交一整套多段配置直接核算，无需登记。
func (s *Server) handleEvaluateArrayAdhoc(w http.ResponseWriter, req *http.Request) {
	var dto arrayConfigDTO
	if !decodeBody(w, req, &dto) {
		return
	}
	cfg, err := arrayConfigFromDTO(dto)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	res, err := array.Evaluate(cfg)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, arrayResultToDTO(res, cfg.Name, "", "", cfg.Feed.MolarMass))
}

// handleEvaluateCaseArray 对已登记工况档套用已登记多段配置：
// 工况档的溶液与进料流量整体作为阵列进料（覆盖配置自带进料），
// 配置里的段列表（各段膜参数与压差）原样生效。
func (s *Server) handleEvaluateCaseArray(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	var dto evaluateCaseArrayDTO
	if !decodeBody(w, req, &dto) {
		return
	}
	if dto.Array == "" {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   validation.CodeNonPositiveParameter,
			Reason: "请求体必须给出要套用的多段配置名 array",
		})
		return
	}
	spec, err := s.registry.Get(name)
	if err != nil {
		if isErr(err, cases.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "case_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	cfg, err := s.arrays.Get(dto.Array)
	if err != nil {
		if isErr(err, array.ErrNotFound) {
			writeError(w, http.StatusNotFound, &validation.DomainError{Code: "array_not_found", Reason: err.Error()})
			return
		}
		writeDomainError(w, err)
		return
	}
	merged := array.WithFeed(cfg, spec.Feed, spec.FeedFlow)
	res, err := array.Evaluate(merged)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, arrayResultToDTO(res, name, dto.Array, name, spec.Feed.MolarMass))
}

// ---- 编解码与错误映射 ----

// decodeBody 解码 JSON 请求体；未知字段与语法错误一律 400 malformed_json。
func decodeBody(w http.ResponseWriter, req *http.Request, v any) bool {
	defer req.Body.Close()
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, &validation.DomainError{
			Code:   "malformed_json",
			Reason: "请求体不是合法 JSON 或含未知字段: " + err.Error(),
		})
		return false
	}
	return true
}

func decodeSpec(w http.ResponseWriter, req *http.Request) (specDTO, bool) {
	var dto specDTO
	if !decodeBody(w, req, &dto) {
		return dto, false
	}
	return dto, true
}

func specFromDTO(dto specDTO) (cases.Spec, error) {
	feed := osmotic.Solution{
		Temperature: dto.Feed.TemperatureK,
		VanTHoff:    dto.Feed.VanTHoff,
	}
	if dto.Feed.Molarity != nil {
		feed.Molarity = *dto.Feed.Molarity
	}
	if dto.Feed.MassConcentration != nil {
		feed.MassConcentration = *dto.Feed.MassConcentration
	}
	if dto.Feed.MolarMass != nil {
		feed.MolarMass = *dto.Feed.MolarMass
	}
	return cases.Spec{
		Name: dto.Name,
		FeedSpec: membrane.FeedSpec{
			Feed:            feed,
			AppliedPressure: dto.AppliedPressureBar,
			FeedFlow:        dto.FeedFlowLh,
			Permeability:    dto.PermeabilityLMH,
			Area:            dto.AreaM2,
			Rejection:       dto.Rejection,
			Polarization:    dto.Polarization,
		},
	}, nil
}

func specToDTO(spec cases.Spec) specDTO {
	return specDTO{
		Name:               spec.Name,
		Feed:               toSolutionDTO(spec.Feed),
		AppliedPressureBar: spec.AppliedPressure,
		FeedFlowLh:         spec.FeedFlow,
		PermeabilityLMH:    spec.Permeability,
		AreaM2:             spec.Area,
		Rejection:          spec.Rejection,
		Polarization:       spec.Polarization,
	}
}

// arrayConfigFromDTO 把请求 DTO 翻成内核配置。stage_count 给了就必须
// 为正且与段列表长度一致——段数为零、负数、与段列表对不上，都在这里拒绝。
func arrayConfigFromDTO(dto arrayConfigDTO) (array.Config, error) {
	if dto.StageCount != nil {
		if *dto.StageCount < 1 {
			return array.Config{}, validation.NewError(validation.CodeInvalidStageCount,
				fmt.Sprintf("段数必须为正整数，实际为 %d", *dto.StageCount))
		}
		if *dto.StageCount != len(dto.Stages) {
			return array.Config{}, validation.NewError(validation.CodeStageCountMismatch,
				fmt.Sprintf("声明段数 %d 与段列表长度 %d 不一致", *dto.StageCount, len(dto.Stages)))
		}
	}
	feed := osmotic.Solution{
		Temperature: dto.Feed.TemperatureK,
		VanTHoff:    dto.Feed.VanTHoff,
	}
	if dto.Feed.Molarity != nil {
		feed.Molarity = *dto.Feed.Molarity
	}
	if dto.Feed.MassConcentration != nil {
		feed.MassConcentration = *dto.Feed.MassConcentration
	}
	if dto.Feed.MolarMass != nil {
		feed.MolarMass = *dto.Feed.MolarMass
	}
	stages := make([]array.StageSpec, len(dto.Stages))
	for i, s := range dto.Stages {
		stages[i] = array.StageSpec{
			Area:            s.AreaM2,
			Permeability:    s.PermeabilityLMH,
			Rejection:       s.Rejection,
			Polarization:    s.Polarization,
			AppliedPressure: s.AppliedPressureBar,
		}
	}
	return array.Config{
		Name:     dto.Name,
		Feed:     feed,
		FeedFlow: dto.FeedFlowLh,
		Stages:   stages,
	}, nil
}

func arrayConfigToDTO(cfg array.Config) arrayConfigDTO {
	count := len(cfg.Stages)
	dto := arrayConfigDTO{
		Name:       cfg.Name,
		StageCount: &count,
		Feed:       toSolutionDTO(cfg.Feed),
		FeedFlowLh: cfg.FeedFlow,
		Stages:     make([]stageSpecDTO, len(cfg.Stages)),
	}
	for i, s := range cfg.Stages {
		dto.Stages[i] = stageSpecDTO{
			AreaM2:             s.Area,
			PermeabilityLMH:    s.Permeability,
			Rejection:          s.Rejection,
			Polarization:       s.Polarization,
			AppliedPressureBar: s.AppliedPressure,
		}
	}
	return dto
}

func arrayResultToDTO(res *array.Result, name, arrayName, caseName string, molarMass float64) arrayResultDTO {
	dto := arrayResultDTO{
		Name:       name,
		ArrayName:  arrayName,
		CaseName:   caseName,
		Status:     res.Status,
		StageCount: res.StageCount,
		Stages:     make([]stageResultDTO, 0, len(res.Stages)),
		Units:      standardUnits(),
	}
	for _, sr := range res.Stages {
		dto.Stages = append(dto.Stages, stageResultToDTO(sr))
	}
	if f := res.Failure; f != nil {
		dto.Failure = &failureDTO{
			StageIndex: f.StageIndex,
			Code:       f.Code,
			Reason:     f.Reason,
			StageFeed: stageFeedSnapshotDTO{
				FeedMolarityMolar:      f.Feed.Molarity,
				FeedFlowLh:             f.Feed.Flow,
				TemperatureK:           f.Feed.Temperature,
				VanTHoff:               f.Feed.VanTHoff,
				AppliedPressureBar:     f.Feed.AppliedPressure,
				FeedOsmoticPressureBar: f.Feed.OsmoticPressure,
				WallOsmoticPressureBar: f.Feed.WallOsmoticPressure,
			},
		}
	}
	if sum := res.Summary; sum != nil {
		dto.Summary = &arraySummaryDTO{
			StageCount:                   sum.StageCount,
			TotalPermeateFlow:            sum.TotalPermeateFlow,
			OverallRecovery:              sum.OverallRecovery,
			FinalBrineFlow:               sum.FinalBrineFlow,
			FinalBrineMolarityMolar:      sum.FinalBrineMolarity,
			BlendedPermeateMolarityMolar: sum.BlendedPermeateMolarity,
			GlobalSalt: globalSaltDTO{
				FeedSaltMolFlow:     sum.Salt.FeedSaltMolFlow,
				PermeateSaltMolFlow: sum.Salt.PermeateSaltMolFlow,
				BrineSaltMolFlow:    sum.Salt.BrineSaltMolFlow,
				ResidualMolFlow:     sum.Salt.ResidualMolFlow,
			},
		}
		if molarMass > 0 {
			dto.Summary.FinalBrineMassConcentration = sum.FinalBrineMolarity * molarMass
			dto.Summary.BlendedPermeateMassConcentration = sum.BlendedPermeateMolarity * molarMass
			dto.Summary.GlobalSalt.FeedSaltMassFlow = sum.Salt.FeedSaltMassFlow
			dto.Summary.GlobalSalt.PermeateSaltMassFlow = sum.Salt.PermeateSaltMassFlow
			dto.Summary.GlobalSalt.BrineSaltMassFlow = sum.Salt.BrineSaltMassFlow
			dto.Summary.GlobalSalt.ResidualMassFlow = sum.Salt.ResidualMassFlow
		}
	}
	return dto
}

func stageResultToDTO(sr array.StageResult) stageResultDTO {
	beta := sr.Spec.Polarization
	if beta == 0 {
		beta = 1 // 与内核一致：缺省按无极化
	}
	dto := stageResultDTO{
		StageIndex: sr.Index,
		Input: stageInputDTO{
			FeedMolarityMolar:  sr.Out.FeedMolarity,
			FeedFlowLh:         sr.Spec.FeedFlow,
			TemperatureK:       sr.Spec.Feed.Temperature,
			VanTHoff:           sr.Spec.Feed.VanTHoff,
			AppliedPressureBar: sr.Spec.AppliedPressure,
			AreaM2:             sr.Spec.Area,
			PermeabilityLMH:    sr.Spec.Permeability,
			Rejection:          sr.Spec.Rejection,
			Polarization:       beta,
		},
		PermeateMolarityMolar:      sr.Out.PermeateMolarity,
		BrineMolarityMolar:         sr.Out.BrineMolarity,
		FeedOsmoticPressureBar:     sr.Out.FeedOsmoticPressure,
		WallOsmoticPressureBar:     sr.Out.WallOsmoticPressure,
		PermeateOsmoticPressureBar: sr.Out.PermeateOsmoticPressure,
		NetDrivingPressureBar:      sr.Out.NetDrivingPressure,
		PermeateFlowLh:             sr.Out.PermeateFlow,
		BrineFlowLh:                sr.Out.BrineFlow,
		Recovery:                   sr.Out.Recovery,
		Salt: saltFlowsDTO{
			FeedSaltMassFlowGh:     sr.Out.Salt.FeedSaltMassFlow,
			PermeateSaltMassFlowGh: sr.Out.Salt.PermeateSaltMassFlow,
			BrineSaltMassFlowGh:    sr.Out.Salt.BrineSaltMassFlow,
			ResidualGh:             sr.Out.Salt.Residual,
		},
	}
	if m := sr.Spec.Feed.MolarMass; m > 0 {
		dto.Input.FeedMassConcentration = sr.Out.FeedMolarity * m
		dto.BrineMassConcentration = sr.Out.BrineMolarity * m
	}
	return dto
}

func outcomeToDTO(name string, spec membrane.FeedSpec, out *membrane.Outcome) outcomeDTO {
	dto := outcomeDTO{
		Name:                       name,
		FeedMolarityMolar:          out.FeedMolarity,
		PermeateMolarityMolar:      out.PermeateMolarity,
		BrineMolarityMolar:         out.BrineMolarity,
		FeedOsmoticPressureBar:     out.FeedOsmoticPressure,
		WallOsmoticPressureBar:     out.WallOsmoticPressure,
		PermeateOsmoticPressureBar: out.PermeateOsmoticPressure,
		NetDrivingPressureBar:      out.NetDrivingPressure,
		PermeateFlowLh:             out.PermeateFlow,
		BrineFlowLh:                out.BrineFlow,
		Recovery:                   out.Recovery,
		Salt: saltFlowsDTO{
			FeedSaltMassFlowGh:     out.Salt.FeedSaltMassFlow,
			PermeateSaltMassFlowGh: out.Salt.PermeateSaltMassFlow,
			BrineSaltMassFlowGh:    out.Salt.BrineSaltMassFlow,
			ResidualGh:             out.Salt.Residual,
		},
		Units: standardUnits(),
	}
	if spec.Feed.MolarMass > 0 {
		m := spec.Feed.MolarMass
		dto.FeedMassConcentration = out.FeedMolarity * m
		dto.BrineMassConcentration = out.BrineMolarity * m
	}
	return dto
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *validation.DomainError) {
	writeJSON(w, status, errorDTO{Error: e.Error(), Code: e.Code, Reason: e.Reason})
}

// writeDomainError 把内核领域错误映射成合适的 HTTP 状态码：
// 工况不合法/不物理一律 400，并把具体原因带回去。
func writeDomainError(w http.ResponseWriter, err error) {
	if de, ok := err.(*validation.DomainError); ok {
		writeError(w, http.StatusBadRequest, de)
		return
	}
	writeError(w, http.StatusInternalServerError, &validation.DomainError{
		Code:   "internal_error",
		Reason: err.Error(),
	})
}

func isErr(err, target error) bool {
	return err != nil && (err == target || isUnwrap(err, target))
}

func isUnwrap(err, target error) bool {
	type unwrapper interface{ Unwrap() error }
	for {
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
		if err == nil {
			return false
		}
		if err == target {
			return true
		}
	}
}
