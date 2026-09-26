package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"
)

// threeStageBody 是一份三段串联配置：膜面积递减、逐段压差不同、部分截留。
const threeStageBody = `{
  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
           "temperature_k": 298.15, "vanth_hoff_factor": 2},
  "feed_flow_lh": 1000,
  "stages": [
    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
     "polarization_factor": 1, "applied_pressure_bar": 12},
    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
     "polarization_factor": 1, "applied_pressure_bar": 13},
    {"area_m2": 12, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
     "polarization_factor": 1, "applied_pressure_bar": 13.5}
  ]
}`

func stagesOf(t *testing.T, resp map[string]any) []any {
	t.Helper()
	stages, ok := resp["stages"].([]any)
	if !ok {
		t.Fatalf("响应缺少 stages 数组: %v", resp)
	}
	return stages
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %s 缺失或不是数值: %v", key, m)
	}
	return v
}

// handMarch 在测试侧按段手工推进：π=i·C·R·T、NDP=Δp−β·πf+πp、Qp=A·Lp·NDP、
// Cb=Cf(1−(1−r)Y)/(1−Y)，上一段浓水直接作为下一段进料。
// 返回每段的 (进料浓度, 浓水浓度, 产水量, 浓水量)。
func handMarch(t *testing.T, cf, qf float64, stages [][2]float64) [][4]float64 {
	t.Helper()
	const R = 0.08314
	const i, temp, r = 2.0, 298.15, 0.98
	var out [][4]float64
	for _, st := range stages {
		area, dp := st[0], st[1]
		piF := i * cf * R * temp
		cp := cf * (1 - r)
		piP := i * cp * R * temp
		ndp := dp - piF + piP
		if ndp <= 0 {
			t.Fatalf("手算推进：NDP=%g 非正", ndp)
		}
		qp := area * 1.8 * ndp
		y := qp / qf
		cb := cf * (1 - (1-r)*y) / (1 - y)
		out = append(out, [4]float64{cf, cb, qp, qf - qp})
		cf, qf = cb, qf-qp
	}
	return out
}

func TestAdhocArrayEvaluate_ThreeStageHandCheck(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", threeStageBody)
	if code != http.StatusOK {
		t.Fatalf("三段串联应 200，实际 %d %v", code, resp)
	}
	if resp["status"] != "ok" {
		t.Fatalf("状态应为 ok，实际 %v", resp["status"])
	}
	if resp["failure"] != nil {
		t.Fatalf("跑通时 failure 应为 null，实际 %v", resp["failure"])
	}
	stages := stagesOf(t, resp)
	if len(stages) != 3 {
		t.Fatalf("应返回 3 段，实际 %d", len(stages))
	}

	// 手工按段推进复算：每段浓水浓度、产水量都要与服务返回对上。
	cf := 5.0 / 58.44
	refs := handMarch(t, cf, 1000, [][2]float64{{36, 12}, {20, 13}, {12, 13.5}})
	var sumQp float64
	for k, raw := range stages {
		st := raw.(map[string]any)
		if idx := num(t, st, "stage_index"); idx != float64(k+1) {
			t.Fatalf("段号应从 1 递增，第 %d 个元素 stage_index=%v", k, idx)
		}
		input := st["input"].(map[string]any)
		if got := num(t, input, "feed_molarity_mol_per_l"); math.Abs(got-refs[k][0]) > 1e-9 {
			t.Fatalf("第 %d 段进料浓度 %g ≠ 手算 %g", k+1, got, refs[k][0])
		}
		if got := num(t, st, "brine_molarity_mol_per_l"); math.Abs(got-refs[k][1]) > 1e-9 {
			t.Fatalf("第 %d 段浓水浓度 %g ≠ 手算 %g", k+1, got, refs[k][1])
		}
		if got := num(t, st, "permeate_flow_lh"); math.Abs(got-refs[k][2]) > 1e-6 {
			t.Fatalf("第 %d 段产水量 %g ≠ 手算 %g", k+1, got, refs[k][2])
		}
		// 段间接线：下一段进料 = 上一段浓水
		if k+1 < len(stages) {
			nextInput := stages[k+1].(map[string]any)["input"].(map[string]any)
			if got := num(t, nextInput, "feed_molarity_mol_per_l"); got != num(t, st, "brine_molarity_mol_per_l") {
				t.Fatalf("第 %d 段进料浓度 %g ≠ 第 %d 段浓水浓度", k+2, got, k+1)
			}
			if got := num(t, nextInput, "feed_flow_lh"); got != num(t, st, "brine_flow_lh") {
				t.Fatalf("第 %d 段进料流量 ≠ 第 %d 段浓水流量", k+2, k+1)
			}
		}
		sumQp += num(t, st, "permeate_flow_lh")
	}

	// 汇总：总产水量、总回收率必须能用逐段结果加总验证一致。
	sum := resp["summary"].(map[string]any)
	if got := num(t, sum, "total_permeate_flow_lh"); math.Abs(got-sumQp) > 1e-9 {
		t.Fatalf("总产水量 %g ≠ 逐段加总 %g", got, sumQp)
	}
	if got := num(t, sum, "overall_recovery"); math.Abs(got-sumQp/1000) > 1e-12 {
		t.Fatalf("总回收率 %g ≠ %g", got, sumQp/1000)
	}
	lastBrine := num(t, stages[2].(map[string]any), "brine_molarity_mol_per_l")
	if got := num(t, sum, "final_brine_molarity_mol_per_l"); got != lastBrine {
		t.Fatalf("末段浓水浓度 %g 应与第 3 段一致 %g", got, lastBrine)
	}

	// 全局盐守恒：阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量。
	var permSalt float64
	for _, raw := range stages {
		st := raw.(map[string]any)
		permSalt += num(t, st, "permeate_molarity_mol_per_l") * num(t, st, "permeate_flow_lh")
	}
	feedSalt := cf * 1000
	brineSalt := lastBrine * num(t, stages[2].(map[string]any), "brine_flow_lh")
	if residual := feedSalt - permSalt - brineSalt; math.Abs(residual) > 1e-9 {
		t.Fatalf("全局盐量不守恒：%g ≠ %g + %g（残差 %g）", feedSalt, permSalt, brineSalt, residual)
	}
	gs := sum["global_salt_balance"].(map[string]any)
	if got := num(t, gs, "residual_mol_per_h"); math.Abs(got) > 1e-9 {
		t.Fatalf("汇总残差 %g 超浮点噪声", got)
	}
	if got := num(t, gs, "feed_salt_mass_flow_gh"); math.Abs(got-5000) > 1e-6 {
		t.Fatalf("进料盐量应为 5000 g/h，实际 %g", got)
	}

	// 混合产水浓度：按产水量加权，不等于算术平均。
	blend := num(t, sum, "blended_permeate_molarity_mol_per_l")
	var arith float64
	for _, raw := range stages {
		arith += num(t, raw.(map[string]any), "permeate_molarity_mol_per_l")
	}
	arith /= 3
	if math.Abs(blend-arith) < 1e-9 {
		t.Fatalf("混合产水浓度 %g 不应等于算术平均 %g", blend, arith)
	}
	if blend != permSalt/sumQp {
		t.Fatalf("混合产水浓度 %g ≠ Σ(Cp·Qp)/ΣQp = %g", blend, permSalt/sumQp)
	}
}

func TestAdhocArrayEvaluate_StageFailureKeepsPrefix(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 中间段压差 1 bar，远低于该段实际进料渗透压（约 8.5 bar）。
	bad := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
	     "polarization_factor": 1, "applied_pressure_bar": 12},
	    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
	     "polarization_factor": 1, "applied_pressure_bar": 1},
	    {"area_m2": 12, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
	     "polarization_factor": 1, "applied_pressure_bar": 13.5}
	  ]
	}`
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", bad)
	if code != http.StatusOK {
		t.Fatalf("段失败仍应 200（结果型响应），实际 %d %v", code, resp)
	}
	if resp["status"] != "stage_failed" {
		t.Fatalf("状态应为 stage_failed，实际 %v", resp["status"])
	}
	if got := num(t, resp, "stage_count"); got != 3 {
		t.Fatalf("stage_count 应为配置总段数 3，实际 %v", got)
	}
	if resp["summary"] != nil {
		t.Fatalf("未跑完全部段不得给汇总，实际 %v", resp["summary"])
	}
	failure := resp["failure"].(map[string]any)
	if got := num(t, failure, "stage_index"); got != 2 {
		t.Fatalf("应定位到第 2 段，实际第 %v 段", got)
	}
	if failure["code"] != "non_positive_ndp" {
		t.Fatalf("错误码应为 non_positive_ndp，实际 %v", failure["code"])
	}
	if failure["reason"] == "" {
		t.Fatal("失败原因不得为空")
	}

	// 失败段之前的结果必须原样留在响应里。
	stages := stagesOf(t, resp)
	if len(stages) != 1 {
		t.Fatalf("应保留第 1 段结果，实际 %d 段", len(stages))
	}
	st1 := stages[0].(map[string]any)
	if got := num(t, st1, "permeate_flow_lh"); got <= 0 {
		t.Fatalf("第 1 段产水量应为正，实际 %g", got)
	}

	// 失败段快照：进料条件就是第 1 段浓水接力过来的工况。
	snap := failure["stage_feed"].(map[string]any)
	if got := num(t, snap, "feed_molarity_mol_per_l"); got != num(t, st1, "brine_molarity_mol_per_l") {
		t.Fatalf("失败段进料浓度 %g 应等于第 1 段浓水 %g", got, num(t, st1, "brine_molarity_mol_per_l"))
	}
	if got := num(t, snap, "feed_flow_lh"); got != num(t, st1, "brine_flow_lh") {
		t.Fatalf("失败段进料流量 %g 应等于第 1 段浓水流量 %g", got, num(t, st1, "brine_flow_lh"))
	}
	if got := num(t, snap, "applied_pressure_bar"); got != 1 {
		t.Fatalf("快照应带失败段压差 1 bar，实际 %g", got)
	}
	if got := num(t, snap, "feed_osmotic_pressure_bar"); got <= 1 {
		t.Fatalf("失败段进料渗透压应高于 1 bar 压差，实际 %g", got)
	}

	// 对照：同一份配置把第 2 段压差改回正常值，第 1 段结果必须逐位一致。
	code, ok := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", threeStageBody)
	if code != http.StatusOK || ok["status"] != "ok" {
		t.Fatalf("正常配置应跑通: %d %v", code, ok)
	}
	okSt1 := stagesOf(t, ok)[0].(map[string]any)
	for _, k := range []string{"brine_molarity_mol_per_l", "permeate_flow_lh", "brine_flow_lh", "recovery"} {
		if num(t, st1, k) != num(t, okSt1, k) {
			t.Fatalf("失败响应里第 1 段字段 %s 被污染：%v ≠ %v", k, st1[k], okSt1[k])
		}
	}
}

func TestArraySingleStage_ParityWithSingleEndpoint(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 同一份工况：一段阵列 vs 单段接口，结果必须逐字段吻合。
	single := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "applied_pressure_bar": 12, "feed_flow_lh": 1000,
	  "permeability_lmh_per_bar": 1.8, "area_m2": 36,
	  "salt_rejection": 0.98, "polarization_factor": 1
	}`
	oneStage := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98,
	     "polarization_factor": 1, "applied_pressure_bar": 12}
	  ]
	}`
	code, solo := doJSON(t, ts, http.MethodPost, "/v1/evaluate", single)
	if code != http.StatusOK {
		t.Fatalf("单段接口异常: %d %v", code, solo)
	}
	code, arr := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", oneStage)
	if code != http.StatusOK || arr["status"] != "ok" {
		t.Fatalf("一段阵列应跑通: %d %v", code, arr)
	}
	stages := stagesOf(t, arr)
	if len(stages) != 1 {
		t.Fatalf("一段阵列应返回 1 段，实际 %d", len(stages))
	}
	st := stages[0].(map[string]any)
	for _, k := range []string{
		"permeate_molarity_mol_per_l", "brine_molarity_mol_per_l",
		"feed_osmotic_pressure_bar", "wall_osmotic_pressure_bar", "permeate_osmotic_pressure_bar",
		"net_driving_pressure_bar", "permeate_flow_lh", "brine_flow_lh", "recovery",
	} {
		if num(t, solo, k) != num(t, st, k) {
			t.Fatalf("字段 %s 不一致：单段 %v ≠ 阵列 %v", k, solo[k], st[k])
		}
	}
	// 进料浓度与盐衡算也逐位一致。
	input := st["input"].(map[string]any)
	if num(t, solo, "feed_molarity_mol_per_l") != num(t, input, "feed_molarity_mol_per_l") {
		t.Fatal("进料浓度不一致")
	}
	soloSalt := solo["salt_balance"].(map[string]any)
	stSalt := st["salt_balance"].(map[string]any)
	for _, k := range []string{"feed_salt_mass_flow_gh", "permeate_salt_mass_flow_gh", "brine_salt_mass_flow_gh", "residual_gh"} {
		if num(t, soloSalt, k) != num(t, stSalt, k) {
			t.Fatalf("盐衡算字段 %s 不一致：%v ≠ %v", k, soloSalt[k], stSalt[k])
		}
	}
	// 汇总：单段阵列的总产水 = 该段产水，总回收率 = 该段回收率。
	sum := arr["summary"].(map[string]any)
	if num(t, sum, "total_permeate_flow_lh") != num(t, solo, "permeate_flow_lh") ||
		num(t, sum, "overall_recovery") != num(t, solo, "recovery") {
		t.Fatal("单段阵列汇总与单段接口不一致")
	}
}

func TestArrayStageCountValidation(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	feed := `"feed": {"molarity_mol_per_l": 0.09, "temperature_k": 298.15, "vanth_hoff_factor": 2},
	         "feed_flow_lh": 1000`
	stage := `{"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 1, "applied_pressure_bar": 12}`

	// 段数为 0 / 负数：直接拒绝。
	for _, tc := range []struct {
		name, body, code string
	}{
		{"段数为零", fmt.Sprintf(`{"stage_count": 0, %s, "stages": [%s]}`, feed, stage), "invalid_stage_count"},
		{"段数为负", fmt.Sprintf(`{"stage_count": -2, %s, "stages": [%s]}`, feed, stage), "invalid_stage_count"},
		{"段数与列表不符", fmt.Sprintf(`{"stage_count": 2, %s, "stages": [%s]}`, feed, stage), "stage_count_mismatch"},
		{"段列表为空", fmt.Sprintf(`{%s, "stages": []}`, feed), "invalid_stage_count"},
		{"不给段列表", fmt.Sprintf(`{%s}`, feed), "invalid_stage_count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d %v", code, resp)
			}
			if resp["code"] != tc.code {
				t.Fatalf("错误码应为 %s，实际 %v", tc.code, resp["code"])
			}
		})
	}

	// 段数与列表一致：正常受理。
	good := fmt.Sprintf(`{"stage_count": 1, %s, "stages": [%s]}`, feed, stage)
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", good)
	if code != http.StatusOK || resp["status"] != "ok" {
		t.Fatalf("段数一致应跑通: %d %v", code, resp)
	}
}

func TestArrayConfigRegistrationAndEvaluate(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 登记（带 name）。
	withName := `{
	  "name": "train-a",
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 12},
	    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 13},
	    {"area_m2": 12, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 13.5}
	  ]
	}`
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays", withName)
	if code != http.StatusCreated {
		t.Fatalf("登记配置失败: %d %v", code, resp)
	}
	if code, _ := doJSON(t, ts, http.MethodPost, "/v1/arrays", withName); code != http.StatusConflict {
		t.Fatalf("重名应 409，实际 %d", code)
	}

	// 列表与取回。
	code, list := doJSON(t, ts, http.MethodGet, "/v1/arrays", "")
	if code != http.StatusOK {
		t.Fatalf("列表失败: %v", list)
	}
	code, got := doJSON(t, ts, http.MethodGet, "/v1/arrays/train-a", "")
	if code != http.StatusOK {
		t.Fatalf("取回失败: %d %v", code, got)
	}
	if num(t, got, "stage_count") != 3 || len(got["stages"].([]any)) != 3 {
		t.Fatalf("取回的配置段数异常: %v", got)
	}

	// 点名评估已登记配置。
	code, res := doJSON(t, ts, http.MethodPost, "/v1/arrays/train-a/evaluate", "")
	if code != http.StatusOK || res["status"] != "ok" {
		t.Fatalf("点名评估失败: %d %v", code, res)
	}
	if len(stagesOf(t, res)) != 3 {
		t.Fatal("应返回 3 段结果")
	}

	// PUT 整体替换（2 段），再评估应只有 2 段。
	twoStage := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 1000,
	  "stages": [
	    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 12},
	    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 13}
	  ]
	}`
	if code, resp := doJSON(t, ts, http.MethodPut, "/v1/arrays/train-a", twoStage); code != http.StatusOK {
		t.Fatalf("PUT 替换失败: %d %v", code, resp)
	}
	code, res = doJSON(t, ts, http.MethodPost, "/v1/arrays/train-a/evaluate", "")
	if code != http.StatusOK || len(stagesOf(t, res)) != 2 {
		t.Fatalf("替换后应评估 2 段: %d %v", code, res)
	}

	// 删除后 404。
	if code, _ := doJSON(t, ts, http.MethodDelete, "/v1/arrays/train-a", ""); code != http.StatusOK {
		t.Fatalf("删除失败: %d", code)
	}
	code, resp = doJSON(t, ts, http.MethodPost, "/v1/arrays/train-a/evaluate", "")
	if code != http.StatusNotFound || resp["code"] != "array_not_found" {
		t.Fatalf("删除后应 404 array_not_found，实际 %d %v", code, resp)
	}

	// 内置三段配置开箱可算。
	code, res = doJSON(t, ts, http.MethodPost, "/v1/arrays/brackish-train-3/evaluate", "")
	if code != http.StatusOK || res["status"] != "ok" || len(stagesOf(t, res)) != 3 {
		t.Fatalf("内置三段配置应开箱可算: %d %v", code, res)
	}
}

func TestCaseEvaluateArray_FeedOverrideRule(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 工况档：0.09 mol/L；配置自带 0.5 mol/L —— 套用时工况档进料必须整体生效。
	caseBody := `{
	  "name": "well-9",
	  "feed": {"molarity_mol_per_l": 0.09, "temperature_k": 303.15, "vanth_hoff_factor": 2},
	  "applied_pressure_bar": 10, "feed_flow_lh": 800,
	  "permeability_lmh_per_bar": 2, "area_m2": 30, "salt_rejection": 1
	}`
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases", caseBody); code != http.StatusCreated {
		t.Fatalf("登记工况档失败: %d %v", code, resp)
	}
	arrayBody := `{
	  "name": "tpl-2",
	  "feed": {"molarity_mol_per_l": 0.5, "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "feed_flow_lh": 100,
	  "stages": [
	    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 1, "applied_pressure_bar": 12},
	    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 1, "applied_pressure_bar": 13}
	  ]
	}`
	if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays", arrayBody); code != http.StatusCreated {
		t.Fatalf("登记配置失败: %d %v", code, resp)
	}

	code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases/well-9/evaluate-array", `{"array": "tpl-2"}`)
	if code != http.StatusOK || resp["status"] != "ok" {
		t.Fatalf("套用评估失败: %d %v", code, resp)
	}
	st1 := stagesOf(t, resp)[0].(map[string]any)["input"].(map[string]any)
	if got := num(t, st1, "feed_molarity_mol_per_l"); got != 0.09 {
		t.Fatalf("工况档进料浓度 0.09 应生效，实际 %g（配置自带的 0.5 不得泄漏）", got)
	}
	if got := num(t, st1, "feed_flow_lh"); got != 800 {
		t.Fatalf("工况档进料流量 800 应生效，实际 %g", got)
	}
	if got := num(t, st1, "temperature_k"); got != 303.15 {
		t.Fatalf("工况档温度 303.15 应生效，实际 %g", got)
	}
	// 段列表来自配置：第 2 段压差 13 bar。
	st2 := stagesOf(t, resp)[1].(map[string]any)["input"].(map[string]any)
	if got := num(t, st2, "applied_pressure_bar"); got != 13 {
		t.Fatalf("第 2 段压差应来自配置（13 bar），实际 %g", got)
	}

	// 未知工况档 / 未知配置：各自 404。
	code, resp = doJSON(t, ts, http.MethodPost, "/v1/cases/ghost/evaluate-array", `{"array": "tpl-2"}`)
	if code != http.StatusNotFound || resp["code"] != "case_not_found" {
		t.Fatalf("未知工况档应 404 case_not_found，实际 %d %v", code, resp)
	}
	code, resp = doJSON(t, ts, http.MethodPost, "/v1/cases/well-9/evaluate-array", `{"array": "ghost"}`)
	if code != http.StatusNotFound || resp["code"] != "array_not_found" {
		t.Fatalf("未知配置应 404 array_not_found，实际 %d %v", code, resp)
	}
}

func TestArrayConfigsStayIndependent(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 两份不同配置：交替 + 并发提交，结果各自稳定、互不串数据。
	mk := func(name string, feedFlow float64, area3 float64) string {
		return fmt.Sprintf(`{
		  "name": %q,
		  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
		           "temperature_k": 298.15, "vanth_hoff_factor": 2},
		  "feed_flow_lh": %g,
		  "stages": [
		    {"area_m2": 36, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 12},
		    {"area_m2": 20, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 13},
		    {"area_m2": %g, "permeability_lmh_per_bar": 1.8, "salt_rejection": 0.98, "applied_pressure_bar": 13.5}
		  ]
		}`, name, feedFlow, area3)
	}
	for _, body := range []string{mk("cfg-x", 1000, 12), mk("cfg-y", 1200, 10)} {
		if code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays", body); code != http.StatusCreated {
			t.Fatalf("登记失败: %d %v", code, resp)
		}
	}
	eval := func(name string) map[string]any {
		code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/"+name+"/evaluate", "")
		if code != http.StatusOK || resp["status"] != "ok" {
			t.Fatalf("评估 %s 失败: %d %v", name, code, resp)
		}
		return resp
	}
	baseX, baseY := eval("cfg-x"), eval("cfg-y")
	totX := num(t, baseX["summary"].(map[string]any), "total_permeate_flow_lh")
	totY := num(t, baseY["summary"].(map[string]any), "total_permeate_flow_lh")
	if totX == totY {
		t.Fatal("两份配置的产水量不应相同（测试前提不成立）")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name, want := "cfg-x", totX
			if i%2 == 1 {
				name, want = "cfg-y", totY
			}
			for j := 0; j < 10; j++ {
				code, resp := doJSON(t, ts, http.MethodPost, "/v1/arrays/"+name+"/evaluate", "")
				if code != http.StatusOK {
					t.Errorf("评估 %s 状态码 %d", name, code)
					return
				}
				got := num(t, resp["summary"].(map[string]any), "total_permeate_flow_lh")
				if got != want {
					t.Errorf("配置 %s 产水量被串改：%g ≠ %g", name, got, want)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestSingleStageEndpointsUnchanged(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	// 加了多段能力后，原有单段登记、点名评估行为不变。
	code, list := doJSON(t, ts, http.MethodGet, "/v1/cases", "")
	if code != http.StatusOK {
		t.Fatalf("列工况档失败: %v", list)
	}
	code, resp := doJSON(t, ts, http.MethodPost, "/v1/cases/brackish-default/evaluate", "")
	if code != http.StatusOK {
		t.Fatalf("内置档核算失败: %d %v", code, resp)
	}
	if pi := num(t, resp, "feed_osmotic_pressure_bar"); pi < 3 || pi > 5 {
		t.Fatalf("内置档渗透压应几个巴，实际 %g", pi)
	}
	if y := num(t, resp, "recovery"); y <= 0 || y >= 1 {
		t.Fatalf("回收率越界: %g", y)
	}

	// 单段非法工况的拒绝口径不变。
	bad := `{
	  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
	           "temperature_k": 298.15, "vanth_hoff_factor": 2},
	  "applied_pressure_bar": 3, "feed_flow_lh": 1000,
	  "permeability_lmh_per_bar": 1.8, "area_m2": 36, "salt_rejection": 1
	}`
	code, resp = doJSON(t, ts, http.MethodPost, "/v1/evaluate", bad)
	if code != http.StatusBadRequest || resp["code"] != "non_positive_ndp" {
		t.Fatalf("单段非法工况拒绝口径变了: %d %v", code, resp)
	}
}

// 确保阵列响应是合法 JSON 且关键判别字段类型稳定（调用方按 status 分支）。
func TestArrayResponseShape(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	code, resp := doJSON(t, ts, http.MethodPost, "/v1/evaluate-array", threeStageBody)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, resp)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["stage_count"].(float64); !ok {
		t.Fatal("stage_count 应为数值")
	}
	if back["failure"] != nil || back["summary"] == nil {
		t.Fatal("ok 时 failure 应为 null、summary 应非空")
	}
	units := back["units"].(map[string]any)
	if units["pressure"] != "bar" {
		t.Fatal("阵列响应也应带单位说明")
	}
}
