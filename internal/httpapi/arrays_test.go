package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
)

// 三段递减膜面积、逐段压差各异的阵列配置（与验收工况一致）：
// 进料 5 g/L NaCl、25 ℃、i=2、1000 L/h；面积 12/10/8 m²，压差 12/11/10 bar。
const threeStageArrayBody = `{
  "name": "accept-3stage",
  "stage_count": 3,
  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
           "temperature_k": 298.15, "vanth_hoff_factor": 2},
  "feed_flow_lh": 1000,
  "stages": [
    {"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 1},
    {"applied_pressure_bar": 11, "permeability_lmh_per_bar": 1.8, "area_m2": 10, "salt_rejection": 1},
    {"applied_pressure_bar": 10, "permeability_lmh_per_bar": 1.8, "area_m2": 8,  "salt_rejection": 1}
  ]
}`

// handStageHTTP 在 HTTP 测试里独立手算一段（不经过服务代码路径）。
type handStageVals struct {
	piF, ndp, qp, qb, y, cb float64
}

func handStageHTTP(cf, qf, dp, area, lp, r float64) handStageVals {
	piF := 2 * cf * osmotic.R * 298.15
	cp := 0.0
	if !membrane.IsFullRejection(r) {
		cp = cf * (1 - r)
	}
	piP := 2 * cp * osmotic.R * 298.15
	ndp := dp - piF + piP
	qp := area * lp * ndp
	y := qp / qf
	qb := qf - qp
	cb := cf / (1 - y)
	if !membrane.IsFullRejection(r) {
		cb = cf * (1 - (1-r)*y) / (1 - y)
	}
	return handStageVals{piF: piF, ndp: ndp, qp: qp, qb: qb, y: y, cb: cb}
}

func handFeedMolarityHTTP() float64 {
	massConc, molarMass := 5.0, 58.44
	return massConc / molarMass
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("响应缺数值字段 %s: %v", key, m)
	}
	return v
}

func stagesOf(t *testing.T, resp map[string]any) []any {
	t.Helper()
	stages, ok := resp["stages"].([]any)
	if !ok {
		t.Fatalf("响应缺 stages 数组: %v", resp)
	}
	return stages
}

func stageAt(t *testing.T, resp map[string]any, i int) map[string]any {
	t.Helper()
	stages := stagesOf(t, resp)
	if i >= len(stages) {
		t.Fatalf("stages 只有 %d 段，取第 %d 段越界", len(stages), i)
	}
	return stages[i].(map[string]any)
}

// outcomeOf 取某段结果里的单段核算部分（与单段接口同构的字段集）。
func outcomeOf(t *testing.T, stage map[string]any) map[string]any {
	t.Helper()
	out, ok := stage["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("段结果缺 outcome: %v", stage)
	}
	return out
}

func TestArrayAdhoc_ThreeStagesHandCalc(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", threeStageArrayBody)
	if code != http.StatusOK {
		t.Fatalf("三段阵列应 200，实际 %d: %v", code, resp)
	}
	if resp["status"] != "ok" {
		t.Fatalf("状态应为 ok，实际 %v", resp["status"])
	}

	// 手工按段推进复算：每段浓水浓度、产水量逐一比对。
	cf, qf := handFeedMolarityHTTP(), 1000.0
	dps := []float64{12, 11, 10}
	areas := []float64{12, 10, 8}
	var totalQp float64
	for i := 0; i < 3; i++ {
		want := handStageHTTP(cf, qf, dps[i], areas[i], 1.8, 1)
		st := stageAt(t, resp, i)
		if int(num(t, st, "stage")) != i+1 {
			t.Fatalf("第 %d 个结果段号应为 %d", i, i+1)
		}
		feed := st["feed"].(map[string]any)
		if num(t, feed, "molarity_mol_per_l") != cf || num(t, feed, "flow_lh") != qf {
			t.Fatalf("第 %d 段进料未衔接上段浓水", i+1)
		}
		out := outcomeOf(t, st)
		if got := num(t, out, "brine_molarity_mol_per_l"); math.Abs(got-want.cb) > 1e-12 {
			t.Fatalf("第 %d 段浓水浓度 %.12g 与手算 %.12g 不符", i+1, got, want.cb)
		}
		if got := num(t, out, "permeate_flow_lh"); math.Abs(got-want.qp) > 1e-9 {
			t.Fatalf("第 %d 段产水量 %.12g 与手算 %.12g 不符", i+1, got, want.qp)
		}
		if got := num(t, out, "net_driving_pressure_bar"); math.Abs(got-want.ndp) > 1e-12 {
			t.Fatalf("第 %d 段 NDP %.12g 与手算 %.12g 不符", i+1, got, want.ndp)
		}
		totalQp += want.qp
		cf, qf = want.cb, want.qb
	}

	// 汇总：总产水量、总回收率必须能用逐段结果加总验证。
	summary := resp["summary"].(map[string]any)
	if got := num(t, summary, "total_permeate_flow_lh"); math.Abs(got-totalQp) > 1e-9 {
		t.Fatalf("汇总产水量 %g ≠ 逐段加总 %g", got, totalQp)
	}
	if got := num(t, summary, "overall_recovery"); math.Abs(got-totalQp/1000) > 1e-12 {
		t.Fatalf("总回收率 %g ≠ ΣQp/Qf1=%g", got, totalQp/1000)
	}
	if got := num(t, summary, "final_brine_molarity_mol_per_l"); math.Abs(got-cf) > 1e-12 {
		t.Fatalf("末段浓水浓度 %g 与手算 %g 不符", got, cf)
	}
	if got := num(t, summary, "final_brine_flow_lh"); math.Abs(got-qf) > 1e-9 {
		t.Fatalf("末段浓水流量 %g 与手算 %g 不符", got, qf)
	}

	// 全局盐量守恒：首段进料盐量 = 各段产水盐量之和 + 末段浓水盐量。
	g := summary["global_salt_balance"].(map[string]any)
	var permSalt float64
	for i := 0; i < 3; i++ {
		salt := outcomeOf(t, stageAt(t, resp, i))["salt_balance"].(map[string]any)
		permSalt += num(t, salt, "permeate_salt_mass_flow_gh")
	}
	feedSalt := num(t, g, "feed_salt_mass_flow_gh")
	brineSalt := num(t, g, "brine_salt_mass_flow_gh")
	if math.Abs(feedSalt-(permSalt+brineSalt)) > 1e-6 {
		t.Fatalf("全局盐账不闭合：进料 %g ≠ 各段产水和 %g + 末段浓水 %g", feedSalt, permSalt, brineSalt)
	}
	if resid := num(t, g, "residual_gh"); math.Abs(resid) > 1e-6 {
		t.Fatalf("全局盐残差应落在浮点噪声内，实际 %g g/h", resid)
	}
}

func TestArrayAdhoc_StageFailureLocatesAndPreserves(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 把第 2 段压差改到 4 bar：低于该段实际进料渗透压（约 5.09 bar）。
	body := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 1},
	    {"applied_pressure_bar": 4,  "permeability_lmh_per_bar": 1.8, "area_m2": 10, "salt_rejection": 1},
	    {"applied_pressure_bar": 10, "permeability_lmh_per_bar": 1.8, "area_m2": 8,  "salt_rejection": 1}
	  ]
	}`
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", body)
	if code != http.StatusBadRequest {
		t.Fatalf("段非法应 400，实际 %d: %v", code, resp)
	}
	if resp["status"] != "stage_failed" {
		t.Fatalf("状态应为 stage_failed，实际 %v", resp["status"])
	}
	if resp["code"] != "non_positive_ndp" {
		t.Fatalf("顶层错误码应为 non_positive_ndp，实际 %v", resp["code"])
	}

	failed := resp["failed"].(map[string]any)
	if int(num(t, failed, "stage")) != 2 {
		t.Fatalf("必须定位到第 2 段，实际 %v", failed["stage"])
	}
	if failed["code"] != "non_positive_ndp" {
		t.Fatalf("失败段错误码应为 non_positive_ndp，实际 %v", failed["code"])
	}
	if failed["reason"] == nil || failed["reason"] == "" {
		t.Fatal("失败必须带原因")
	}

	// 失败段进料条件 = 第 1 段浓水（手算）。
	want1 := handStageHTTP(handFeedMolarityHTTP(), 1000, 12, 12, 1.8, 1)
	failedFeed := failed["feed"].(map[string]any)
	if got := num(t, failedFeed, "molarity_mol_per_l"); math.Abs(got-want1.cb) > 1e-12 {
		t.Fatalf("失败段进料浓度 %g 应等于第 1 段浓水 %g", got, want1.cb)
	}
	if got := num(t, failedFeed, "flow_lh"); math.Abs(got-want1.qb) > 1e-9 {
		t.Fatalf("失败段进料流量 %g 应等于第 1 段浓水流量 %g", got, want1.qb)
	}

	// 前面已跑通的段：中间结果原样保留，不得清空。
	stages := stagesOf(t, resp)
	if len(stages) != 1 {
		t.Fatalf("失败前应有且仅有第 1 段结果，实际 %d 段", len(stages))
	}
	out1 := outcomeOf(t, stages[0].(map[string]any))
	if got := num(t, out1, "permeate_flow_lh"); math.Abs(got-want1.qp) > 1e-9 {
		t.Fatalf("保留的第 1 段产水量 %g 与手算 %g 不符", got, want1.qp)
	}
	if got := num(t, out1, "brine_molarity_mol_per_l"); math.Abs(got-want1.cb) > 1e-12 {
		t.Fatalf("保留的第 1 段浓水浓度 %g 与手算 %g 不符", got, want1.cb)
	}
	if _, present := resp["summary"]; present {
		t.Fatal("有段失败时不得给出全阵列汇总")
	}
}

func TestArrayAdhoc_SingleStageMatchesSingleEvaluate(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	feed := `"feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2}`
	singleBody := `{` + feed + `,
	  "applied_pressure_bar": 12, "feed_flow_lh": 1000,
	  "permeability_lmh_per_bar": 1.8, "area_m2": 12,
	  "salt_rejection": 1, "polarization_factor": 1}`
	code, single := doJSON(t, ts, http.MethodPost, "/v1/evaluate", singleBody)
	if code != http.StatusOK {
		t.Fatalf("单段核算失败: %d %v", code, single)
	}

	arrayBody := `{` + feed + `, "feed_flow_lh": 1000,
	  "stages": [{"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8,
	              "area_m2": 12, "salt_rejection": 1, "polarization_factor": 1}]}`
	code, arr := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", arrayBody)
	if code != http.StatusOK {
		t.Fatalf("单段阵列核算失败: %d %v", code, arr)
	}

	// 段数为一：逐字段比对，必须与单段接口结果完全一致。
	st := outcomeOf(t, stageAt(t, arr, 0))
	fields := []string{
		"feed_molarity_mol_per_l", "permeate_molarity_mol_per_l", "brine_molarity_mol_per_l",
		"feed_osmotic_pressure_bar", "wall_osmotic_pressure_bar", "permeate_osmotic_pressure_bar",
		"net_driving_pressure_bar", "permeate_flow_lh", "brine_flow_lh", "recovery",
		"feed_mass_concentration_g_per_l", "brine_mass_concentration_g_per_l",
	}
	for _, f := range fields {
		if num(t, st, f) != num(t, single, f) {
			t.Fatalf("字段 %s 不一致：阵列 %v ≠ 单段 %v", f, st[f], single[f])
		}
	}
	arrSalt := st["salt_balance"].(map[string]any)
	singleSalt := single["salt_balance"].(map[string]any)
	for _, f := range []string{"feed_salt_mass_flow_gh", "permeate_salt_mass_flow_gh", "brine_salt_mass_flow_gh", "residual_gh"} {
		if num(t, arrSalt, f) != num(t, singleSalt, f) {
			t.Fatalf("盐账字段 %s 不一致：阵列 %v ≠ 单段 %v", f, arrSalt[f], singleSalt[f])
		}
	}
}

func TestArrayAdhoc_InvalidStageCountAndMissingParams(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	base := `"feed": {"molarity_mol_per_l": 0.1, "temperature_k": 298.15, "vanth_hoff_factor": 2},
	        "feed_flow_lh": 1000`

	// 段数为零 / 为负 / 声明与实际不符。
	oneStage := `"stages": [{"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 1}]`
	for _, body := range []string{
		`{` + base + `, "stages": []}`,
		`{` + base + `, "stage_count": 0, ` + oneStage + `}`,
		`{` + base + `, "stage_count": -1, ` + oneStage + `}`,
		`{` + base + `, "stage_count": 2, ` + oneStage + `}`,
	} {
		code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", body)
		if code != http.StatusBadRequest || resp["code"] != "invalid_stage_count" {
			t.Fatalf("应 400 invalid_stage_count，实际 %d %v", code, resp)
		}
	}

	// 缺段级膜参数：必须指出是第几段。
	missing := `{` + base + `, "stages": [
	  {"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 1},
	  {"applied_pressure_bar": 11, "permeability_lmh_per_bar": 1.8, "salt_rejection": 1}
	]}`
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", missing)
	if code != http.StatusBadRequest || resp["code"] != "missing_stage_parameter" {
		t.Fatalf("缺膜面积应 400 missing_stage_parameter，实际 %d %v", code, resp)
	}
	reason, _ := resp["reason"].(string)
	if !strings.Contains(reason, "第 2 段") {
		t.Fatalf("缺参原因必须指出段号，实际 %q", reason)
	}
}

func TestArrayRegisterEvaluateOnCaseAndIsolation(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 登记两份不同的阵列配置。
	arrA := `{"name": "arr-a", "stages": [
	  {"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 1},
	  {"applied_pressure_bar": 11, "permeability_lmh_per_bar": 1.8, "area_m2": 10, "salt_rejection": 1}
	]}`
	arrB := `{"name": "arr-b", "stages": [
	  {"applied_pressure_bar": 14, "permeability_lmh_per_bar": 2.5, "area_m2": 20, "salt_rejection": 0.98}
	]}`
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays", arrA); code != http.StatusCreated {
		t.Fatalf("登记 arr-a 失败: %d %v", code, resp)
	}
	if code, _ := doJSON(t, ts, http.MethodPost, "/v1/arrays", arrA); code != http.StatusConflict {
		t.Fatalf("重名应 409，实际 %d", code)
	}
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays", arrB); code != http.StatusCreated {
		t.Fatalf("登记 arr-b 失败: %d %v", code, resp)
	}
	if code, resp := doJSON(t, ts, http.MethodGet, "/v1/arrays", ""); code != http.StatusOK {
		t.Fatalf("列出配置失败: %d %v", code, resp)
	}
	if code, resp := doJSON(t, ts, http.MethodGet, "/v1/arrays/arr-a", ""); code != http.StatusOK {
		t.Fatalf("取回配置失败: %d %v", code, resp)
	} else if int(num(t, resp, "stage_count")) != 2 {
		t.Fatalf("arr-a 应为 2 段，实际 %v", resp["stage_count"])
	}

	// 对已登记工况档套用已登记配置；反复交替提交，结果必须稳定且互不串数据。
	evalBody := `{"case_name": "brackish-default"}`
	var prevA, prevB map[string]any
	for round := 0; round < 3; round++ {
		code, a := doJSON(t, ts, http.MethodPost, "/v1/arrays/arr-a/evaluate", evalBody)
		if code != http.StatusOK || a["status"] != "ok" {
			t.Fatalf("arr-a 核算失败: %d %v", code, a)
		}
		code, b := doJSON(t, ts, http.MethodPost, "/v1/arrays/arr-b/evaluate", evalBody)
		if code != http.StatusOK || b["status"] != "ok" {
			t.Fatalf("arr-b 核算失败: %d %v", code, b)
		}
		if len(stagesOf(t, a)) != 2 || len(stagesOf(t, b)) != 1 {
			t.Fatal("段数与登记配置不符")
		}
		if prevA != nil {
			ja, _ := json.Marshal(a["summary"])
			jp, _ := json.Marshal(prevA["summary"])
			if string(ja) != string(jp) {
				t.Fatal("同一配置反复核算结果漂移")
			}
			jb, _ := json.Marshal(b["summary"])
			jpb, _ := json.Marshal(prevB["summary"])
			if string(jb) != string(jpb) {
				t.Fatal("交替提交后配置 b 的结果被串改")
			}
		}
		prevA, prevB = a, b
	}
	// 两份配置结果必须不同（不同段数/参数）。
	sa := prevA["summary"].(map[string]any)
	sb := prevB["summary"].(map[string]any)
	if num(t, sa, "total_permeate_flow_lh") == num(t, sb, "total_permeate_flow_lh") {
		t.Fatal("两份不同配置的结果不应相同")
	}

	// 未登记的配置 / 工况档：404。
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/ghost/evaluate", evalBody); code != http.StatusNotFound || resp["code"] != "array_not_found" {
		t.Fatalf("未知配置应 404 array_not_found，实际 %d %v", code, resp)
	}
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/arr-a/evaluate", `{"case_name": "ghost"}`); code != http.StatusNotFound || resp["code"] != "case_not_found" {
		t.Fatalf("未知工况档应 404 case_not_found，实际 %d %v", code, resp)
	}

	// 删除后不可再点名。
	if code, _ := doJSON(t, ts, http.MethodDelete, "/v1/arrays/arr-b", ""); code != http.StatusOK {
		t.Fatalf("删除配置失败: %d", code)
	}
	if code, _ := doJSON(t, ts, http.MethodPost, "/v1/arrays/arr-b/evaluate", evalBody); code != http.StatusNotFound {
		t.Fatalf("删除后应 404，实际 %d", code)
	}
}

func TestSingleStageEndpointsUnchangedAfterArrays(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 原有单段能力：登记、点名评估、临时评估，行为不变。
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases/brackish-default/evaluate", "")
	if code != http.StatusOK {
		t.Fatalf("内置档点名评估失败: %d %v", code, resp)
	}
	if y := num(t, resp, "recovery"); y <= 0 || y >= 1 {
		t.Fatalf("回收率越界: %v", y)
	}
	if pi := num(t, resp, "feed_osmotic_pressure_bar"); pi < 3 || pi > 5 {
		t.Fatalf("渗透压应几个巴，实际 %v", pi)
	}

	adhoc := `{"feed": {"molarity_mol_per_l": 0.1, "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "applied_pressure_bar": 12, "feed_flow_lh": 1000,
	  "permeability_lmh_per_bar": 1.8, "area_m2": 36, "salt_rejection": 1}`
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate", adhoc); code != http.StatusOK {
		t.Fatalf("临时单段评估失败: %d %v", code, resp)
	}

	register := `{"name": "still-works",
	  "feed": {"molarity_mol_per_l": 0.1, "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "applied_pressure_bar": 12, "feed_flow_lh": 1000,
	  "permeability_lmh_per_bar": 1.8, "area_m2": 36, "salt_rejection": 1}`
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases", register); code != http.StatusCreated {
		t.Fatalf("单段登记失败: %d %v", code, resp)
	}
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases/still-works/evaluate", ""); code != http.StatusOK {
		t.Fatalf("新登记档点名评估失败: %d %v", code, resp)
	}
}

func TestArrayAdhoc_WeightedPermeateAndOverridesHTTP(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 部分截留 + 第 2 段温度覆盖：验证加权产水浓度与覆盖优先级在 HTTP 层可见。
	body := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"applied_pressure_bar": 12, "permeability_lmh_per_bar": 1.8, "area_m2": 12, "salt_rejection": 0.9},
	    {"applied_pressure_bar": 11, "permeability_lmh_per_bar": 1.8, "area_m2": 10, "salt_rejection": 0.9,
	     "temperature_k": 318.15},
	    {"applied_pressure_bar": 10, "permeability_lmh_per_bar": 1.8, "area_m2": 8,  "salt_rejection": 0.9}
	  ]
	}`
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/evaluate", body)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %v", code, resp)
	}

	// 覆盖优先级：第 2 段 318.15 K，其余段回到阵列共用 298.15 K。
	for i, wantT := range []float64{298.15, 318.15, 298.15} {
		feed := stageAt(t, resp, i)["feed"].(map[string]any)
		if got := num(t, feed, "temperature_k"); got != wantT {
			t.Fatalf("第 %d 段温度应为 %g，实际 %g", i+1, wantT, got)
		}
	}

	// 加权平均：Σ(Cp·Qp)/ΣQp，且不等于算术平均。
	var sumCpQp, sumQp, sumCp float64
	for i := 0; i < 3; i++ {
		out := outcomeOf(t, stageAt(t, resp, i))
		cp := num(t, out, "permeate_molarity_mol_per_l")
		qp := num(t, out, "permeate_flow_lh")
		sumCpQp += cp * qp
		sumQp += qp
		sumCp += cp
	}
	summary := resp["summary"].(map[string]any)
	weighted := num(t, summary, "weighted_permeate_molarity_mol_per_l")
	if math.Abs(weighted-sumCpQp/sumQp) > 1e-12 {
		t.Fatalf("加权产水浓度 %g ≠ Σ(Cp·Qp)/ΣQp=%g", weighted, sumCpQp/sumQp)
	}
	if math.Abs(weighted-sumCp/3) < 1e-9 {
		t.Fatal("加权浓度不应退化为算术平均")
	}
}
