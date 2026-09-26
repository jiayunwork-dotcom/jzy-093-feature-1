# rocalc — 苦咸水反渗透（RO）膜过程核算服务

一个常驻的 HTTP 计算服务，管反渗透里的三件事：

1. **渗透压**：van't Hoff 关系 π = i·C·R·T；
2. **单段膜通量与盐衡算**：净推动力 NDP、产水流量 Qp、回收率 Y、浓水浓度 Cb、全系统盐物料守恒；
3. **多段串联阵列编排**：前一段的浓水直接作为后一段的进料，逐段推进到末段，返回每段中间结果与整套阵列的汇总（总产水量、总回收率、末段浓水浓度、全局盐守恒）。

没有页面，不是水费账单或工单系统。给它一份**具名工况档**（可反复点名计算），或临时拼一份工况直接提交，都返回完整结果；工况不合法时带稳定错误码和中文原因返回。

## 物理模型与单位约定（全服务统一，不做隐式量纲换算）

| 量 | 单位 |
|---|---|
| 压力：渗透压、工作压差、NDP | **bar** |
| 摩尔浓度 | mol/L |
| 质量浓度 | g/L；摩尔质量 g/mol |
| 温度 | K（必须为正） |
| 流量 | L/h |
| 膜面积 | m²；渗透系数 Lp | LMH = L/(m²·h)/bar |

气体常数 R = 0.08314 L·bar/(mol·K)。

公式：

- 渗透压：`π = i·C·R·T`（NaCl 取 i=2）。浓度可直接给 mol/L，也可给 g/L + g/mol 换算；两种口径同时给时必须自洽（相对容差 1e-3），否则拒绝。
- 净推动力：`NDP = Δp − β·πf + πp`，β 为浓差极化因子（β≥1，1 表示无极化）。这里的 Δp 是**已经扣过产水背压的工作压差**，全链路只在 bar 量纲下相减。
- 产水流量：`Qp = A·Lp·NDP`（L/h）。
- 回收率：`Y = Qp/Qf`，必须落在**开区间 (0,1)**。
- 产水浓度：完全截留 r=1 时 Cp=0；r<1 时 Cp = Cf·(1−r)。
- 浓水浓度（由盐守恒推导，回收越高浓水越浓）：
  - r=1 极限：`Cb = Cf/(1−Y)`
  - r<1：`Cb = Cf·[1−(1−r)·Y]/(1−Y)`
- 物料守恒：`Cf·Qf = Cp·Qp + Cb·Qb`，其中 Qb = Qf − Qp。服务对每个结果都做闭合校验（摩尔口径必校，给了摩尔质量再校质量口径），不闭合直接报错。

> 明确防守的坑：浓水浓度**绝不能**写成 `Cf·(1−Y)`——那样回收越高浓水反而越稀，盐守恒必然崩塌。

## 多段串联阵列

单段核算解决的是"一根膜管"；多段阵列解决的是"一整串膜管"：第 1 段吃阵列级进料，之后**每一段的进料就是上一段的浓水**（浓度、流量逐项接力，温度与范特霍夫因子随阵列级工况不变），段与段之间压力和流量都在变，服务自己把段串起来推进，不再甩给调用方拼接。

**编排约定（对外契约，可预期、可验证）：**

- **段号从 1 开始**；段数 = 段列表长度。`stage_count` 可省略，给了就必须为正且与段列表长度一致——段数为 0、负数（`invalid_stage_count`）或与列表对不上（`stage_count_mismatch`）直接 400。
- **段参数必须逐段给全**：膜面积、渗透系数、截留率、工作压差缺省（零值）**不会沿用上一段**，而是在该段求值时被校验拒绝并带段号报错，绝不默认成零静默算出异常结果。唯一例外是 `polarization_factor`，与单段一致，缺省按 1（无极化）。
- **阵列级进料是唯一进料来源**：整体进料的浓度、温度、范特霍夫因子、进料流量在阵列级给一次，段级没有覆盖入口——第 k>1 段的进料由第 k−1 段的浓水接力得到，不存在"整体与段级覆盖冲突"的情形。对已登记工况档套用已登记配置时（见下），**工况档的溶液与进料流量整体覆盖配置自带的阵列进料**，段列表（各段膜参数与压差）原样生效。
- **段失败不清空前缀**：某一段被判非法工况（如该段压差低于其实际进料渗透压）时，响应仍是 200，但 `status` 为 `"stage_failed"`：`stages` 里**原样保留失败段之前已跑通的全部中间结果**，`failure` 给出第一段失败的**段号、错误码、原因和该段进料条件**（浓度、流量、压差、渗透压），`summary` 为 null。全部段跑通时 `status` 为 `"ok"`，`failure` 为 null，`summary` 非空。调用方按 `status` 分支即可。
- **段数为 1 时**，阵列结果与单段接口对同一份工况的结果**逐位一致**（走的是同一个内核、同一份输入）。

**两笔独立的盐账（各自校验通过才算数）：**

1. 单段内部：`Cf·Qf = Cp·Qp + Cb·Qb`，每段内核强制闭合；
2. 全局（跨全部段）：`阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量`，由阵列层独立复算并校验，不因为各单段守恒已过就默认成立。残差随 `summary.global_salt_balance` 返回（摩尔口径必校，给了摩尔质量再校质量口径），正常在 1e-12 量级。

**混合产水浓度**按各段实际产水量加权：`Cp_blend = Σ(Cp,k·Qp,k) / Σ Qp,k`，**不是**各段产水浓度的算术平均（各段进料浓度不同，产水浓度和产水量都不同，算术平均必错）。

## 非法工况（HTTP 400，带 code + reason）

- 浓度为负 / 温度不为正 / van't Hoff 因子不为正 / 质量与摩尔口径不自洽；
- 工作压差、进料流量、膜面积、渗透系数不为正，极化因子 <1，截留率不在 (0,1]；
- `non_positive_ndp`：工作压差已低于膜面渗透压（NDP<0），无论膜面积、渗透系数多大，**绝不报告正通量**；
- `invalid_recovery`：回收率 Y 不在 (0,1)；
- `salt_balance_violation`：进料盐量 ≠ 产水盐量 + 浓水盐量。

交叉关系也有测试钉住：升温则 π 升、同压差下 NDP 降；回收上升则 Cb 单调上升；r=1 时产水零盐；Δp 恰好等于 πf 且无极化时 Qp 精确为 0。

## 内置苦咸水工况档 `brackish-default`

进料 5 g/L NaCl（M=58.44 g/mol），25 ℃，i=2；Δp=12 bar，Qf=1000 L/h，A=36 m²，Lp=1.8 LMH/bar，r=1，β=1。

手算对照（服务实跑一致）：

| 量 | 手算 | 服务结果 |
|---|---|---|
| C | 5/58.44 = 0.08556 mol/L | 0.08556 mol/L |
| πf | 2×0.08556×0.08314×298.15 | **4.2416 bar**（几个巴量级） |
| NDP | 12 − 4.2416 | 7.7584 bar |
| Qp | 36×1.8×7.7584 | 502.74 L/h |
| Y | 502.74/1000 | 0.5027 |
| Cb | Cf/(1−Y) | 0.17206 mol/L = 10.055 g/L |
| 盐守恒 | 进料 5000 g/h = 0 + 浓水 5000 g/h | 残差 ≈ 1e-12 |

## HTTP 接口

固定监听 `:8080`（可用环境变量 `RO_PORT` 覆盖）。JSON 请求/响应，字段名自带单位。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查 |
| GET | `/v1/constants` | R 取值与全部公式 |
| GET | `/v1/cases` | 列出已登记工况档 |
| POST | `/v1/cases` | 登记具名工况档（重名 409） |
| PUT | `/v1/cases/{name}` | 登记或整体替换 |
| GET | `/v1/cases/{name}` | 取回工况档 |
| DELETE | `/v1/cases/{name}` | 删除 |
| POST | `/v1/cases/{name}/evaluate` | 点名计算（单段） |
| POST | `/v1/cases/{name}/evaluate-array` | 对该工况档套用已登记多段配置，请求体 `{"array": "<配置名>"}` |
| POST | `/v1/evaluate` | 临时工况直接算（单段），无需登记 |
| POST | `/v1/evaluate-array` | 临时提交一整套多段配置直接算，无需登记 |
| GET | `/v1/arrays` | 列出已登记多段配置 |
| POST | `/v1/arrays` | 登记具名多段配置（重名 409） |
| PUT | `/v1/arrays/{name}` | 登记或整体替换 |
| GET | `/v1/arrays/{name}` | 取回多段配置 |
| DELETE | `/v1/arrays/{name}` | 删除 |
| POST | `/v1/arrays/{name}/evaluate` | 点名已登记多段配置做整套阵列核算 |

多段配置请求体（`POST /v1/arrays`、`POST /v1/evaluate-array` 同构）：

```json
{
  "name": "my-train",
  "stage_count": 3,
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
}
```

阵列响应（跑通时）：`status: "ok"`，`stages[]` 逐段给出**生效输入回显**（`input`：该段进料浓度/流量/压差/膜参数）与**单段内核输出**（渗透压、NDP、产水/浓水流量、回收率、浓水浓度、段内盐衡算），`summary` 给出总产水量、总回收率、末段浓水浓度/流量、流量加权的混合产水浓度和全局盐守恒。段失败时 `status: "stage_failed"`，`failure.stage_index` 定位段号，`failure.stage_feed` 给出该段进料条件，失败段之前的 `stages` 原样保留。

点名内置三段配置（与内置工况档同一份进料，三段面积递减、压差逐段微升）：

```bash
curl -s -X POST http://localhost:8080/v1/arrays/brackish-train-3/evaluate
```

对已登记工况档套用已登记多段配置（工况档的溶液与进料流量整体生效）：

```bash
curl -s -X POST http://localhost:8080/v1/cases/brackish-default/evaluate-array \
  -H 'Content-Type: application/json' -d '{"array": "brackish-train-3"}'
```

段失败响应示例（第 2 段压差 5 bar 低于该段进料渗透压约 8.54 bar）：

```json
{
  "status": "stage_failed",
  "stage_count": 3,
  "stages": [ {"stage_index": 1, "...": "第 1 段结果原样保留"} ],
  "failure": {
    "stage_index": 2,
    "code": "non_positive_ndp",
    "reason": "工作压差 5 bar 低于膜面渗透压 8.5377 bar（β=1），NDP=-3.367 bar，无法产水",
    "stage_feed": {"feed_molarity_mol_per_l": 0.17221, "feed_flow_lh": 491.76,
                   "applied_pressure_bar": 5, "feed_osmotic_pressure_bar": 8.5377}
  },
  "summary": null
}
```

其中 `stage_count` 始终是配置的总段数；`failure.stage_index` 指出推进到哪一段停的。

点名内置档：

```bash
curl -s -X POST http://localhost:8080/v1/cases/brackish-default/evaluate
```

临时工况（部分截留 r=0.98，产水带盐，自动校验守恒）：

```bash
curl -s -X POST http://localhost:8080/v1/evaluate -H 'Content-Type: application/json' -d '{
  "feed": {"mass_concentration_g_per_l": 5, "molar_mass_g_per_mol": 58.44,
           "temperature_k": 298.15, "vanth_hoff_factor": 2},
  "applied_pressure_bar": 12, "feed_flow_lh": 1000,
  "permeability_lmh_per_bar": 1.8, "area_m2": 36,
  "salt_rejection": 0.98}'
```

错误响应示例：

```json
{"error":"non_positive_ndp: ...","code":"non_positive_ndp","reason":"工作压差 3 bar 低于膜面渗透压 4.24 bar ..."}
```

## 代码结构（按职责拆包）

```
cmd/server/              启动入口（信号优雅关停）
internal/osmotic/        van't Hoff 渗透压、质量/摩尔浓度换算
internal/membrane/       types.go 输入输出类型
                         flux.go    NDP、Qp、回收率、Evaluate 编排
                         balance.go 浓水浓度、产水浓度、盐守恒校验
internal/array/          array.go    多段配置与结果类型、编排契约
                         evaluate.go 逐段推进、段间接力、全局盐守恒独立校验
                         registry.go 具名多段配置登记/取用（带锁、取值返副本、段列表深拷贝）
                         builtin.go  内置三段串联配置 brackish-train-3
internal/cases/          具名工况档登记/取用（带锁、取值返副本）、内置苦咸水档
internal/validation/     输入合法性检查与稳定错误码
internal/httpapi/        net/http 接口、DTO、错误码到 HTTP 状态码映射
```

多段编排只在 `internal/array` 里组合单段内核 `membrane.Evaluate`，不改内核、不另起计算路径；登记表按名取**副本**计算、不缓存结果，因此同一进程里多份工况档、多份多段配置交替或并发评估，结果各自独立，不会串名或互相污染（有并发测试覆盖）。

## 开发与测试

```bash
go test ./... -count=1     # 全部自动化测试
go test -race ./...        # 竞态检测
go vet ./... && gofmt -l . # 静态检查与格式
go run ./cmd/server        # 本地启动 :8080
```

测试重点：
- `TestEvaluate_BelowOsmoticPressure_NoPositiveFlux` —— 压差低于渗透压时绝不冒正通量；
- `TestEvaluate_PartialRejection_SaltBalance` / `TestCheckBalance_MolarClosure` —— 进料盐量 = 产水 + 浓水，且错写成 Cf·(1−Y) 必被守恒校验抓住；
- `TestRegistry_TwoCasesStayIndependent` —— 两份工况档互不串档；
- `TestEvaluate_ThreeStages_HandMarch` / `TestAdhocArrayEvaluate_ThreeStageHandCheck` —— 三段递减面积、逐段不同压差，手工按段推进复算，逐段浓水浓度与产水量、总产水量、总回收率全部对账；
- `TestEvaluate_StageFailure_PreservesPrefix` —— 中间段压差不足时定位段号、保留前缀段结果、给出失败段进料条件；
- `TestEvaluate_SingleStage_MatchesKernel` / `TestArraySingleStage_ParityWithSingleEndpoint` —— 段数为 1 时与单段接口逐位一致；
- `TestEvaluate_GlobalSaltBalance` —— 跨全部段的全局盐量守恒，残差落在浮点噪声内；
- `TestEvaluate_BlendedPermeateIsFlowWeighted` —— 混合产水浓度按产水量加权，不退化成算术平均；
- `TestRegistry_ConcurrentAlternatingEvalsStayIndependent` / `TestArrayConfigsStayIndependent` —— 多份多段配置交替、并发评估互不串数据。

## Docker

多阶段构建：构建阶段先 `go test ./...`（测试随仓库一起在容器内跑通），再静态编译；运行阶段用 distroless 非 root 镜像，暴露固定端口 8080。

```bash
docker build -t rocalc:latest .
docker run --rm -p 8080:8080 rocalc:latest
```

Go 1.22，仅用标准库，无第三方依赖。
