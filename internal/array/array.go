// Package array 在 membrane 单段核算内核之上编排“多段串联阵列”：
//
// 第 1 段吃阵列级进料；之后每一段把上一段算出的浓水原样接成本段进料
// （浓度、流量逐项传递，温度与范特霍夫因子随阵列级工况不变），逐段推进，
// 直到末段跑通（StatusOK）或某一段被判非法（StatusStageFailed）。
//
// 对外契约（与 HTTP 层文档一致，调用方可依赖）：
//   - 段号从 1 开始；
//   - 段参数必须逐段给全：膜面积、渗透系数、截留率、工作压差缺省（零值）
//     不会沿用上一段，而是在该段求值时被内核校验拒绝，并带回段号与原因；
//     唯一例外是浓差极化因子，与单段内核一致，零值按 1（无极化）处理；
//   - 阵列级进料工况（浓度、温度、范特霍夫因子、进料流量）是唯一进料来源，
//     段级没有覆盖入口：第 k>1 段的进料就是第 k−1 段的浓水，不存在冲突；
//   - 某段失败不会清空前面已跑通段的结果：Result.Stages 保留失败段之前的
//     全部中间结果，Failure 指出第一段失败的段号、原因与该段进料条件；
//   - 全部段跑通后才给 Summary；其中的全局盐守恒（阵列进料盐量 =
//     各段产水盐量之和 + 末段浓水盐量）由本包独立复算校验，
//     不依赖单段守恒已经通过的结论。
package array

import (
	"rocalc/internal/membrane"
	"rocalc/internal/osmotic"
)

// 阵列推进结果的两种状态。
const (
	// StatusOK 全部段跑通，Summary 非 nil。
	StatusOK = "ok"
	// StatusStageFailed 某一段被判非法工况，推进在该段停止：
	// Stages 保留之前各段结果，Failure 非 nil，Summary 为 nil。
	StatusStageFailed = "stage_failed"
)

// StageSpec 是阵列中一段膜的配置。
//
// 膜面积、渗透系数、截留率、工作压差必须逐段显式给全：缺省（零值）
// 不会沿用上一段的设定，而是在该段求值时被校验拒绝（带段号报错）。
// Polarization 是唯一例外：与单段内核一致，零值按 1（无极化）处理。
type StageSpec struct {
	// Area 膜面积 A，m²，必须为正。
	Area float64
	// Permeability 膜渗透系数 Lp，LMH = L/(m²·h)/bar，必须为正。
	Permeability float64
	// Rejection 盐截留率 r，落在 (0,1]。
	Rejection float64
	// Polarization 浓差极化因子 β，>= 1；零值按 1（无极化）处理。
	Polarization float64
	// AppliedPressure 该段工作压差 Δp，bar，必须为正。段间可各自不同。
	AppliedPressure float64
}

// Config 是一整套多段串联阵列配置。
type Config struct {
	// Name 登记用名称（临时评估可为空）。
	Name string

	// Feed 阵列级进料工况：浓度（摩尔或质量口径）、温度、范特霍夫因子，
	// 可选摩尔质量（用于质量口径盐衡算）。整套阵列共用这一份进料，
	// 段级没有覆盖入口；第 k>1 段的进料由第 k−1 段的浓水接力得到。
	Feed osmotic.Solution

	// FeedFlow 阵列进料流量 Qf,1，L/h，必须为正。
	FeedFlow float64

	// Stages 段列表，顺序即串联顺序；段数 = len(Stages)，必须 >= 1。
	Stages []StageSpec
}

// StageResult 是一段跑通后的完整中间结果：生效输入 + 单段内核输出。
type StageResult struct {
	// Index 段号，从 1 开始。
	Index int
	// Spec 该段实际生效的单段输入（进料已是上一段浓水接力后的工况）。
	Spec membrane.FeedSpec
	// Out 单段内核输出（与 membrane.Evaluate 直接返回的一致）。
	Out membrane.Outcome
}

// StageFeed 是某一段开算前的进料条件快照，用于失败定位与复核。
type StageFeed struct {
	Molarity            float64 // 进料摩尔浓度，mol/L
	Flow                float64 // 进料流量，L/h
	Temperature         float64 // 温度，K
	VanTHoff            float64 // 范特霍夫因子
	AppliedPressure     float64 // 该段设定的工作压差，bar
	OsmoticPressure     float64 // 该段进料渗透压 πf，bar
	WallOsmoticPressure float64 // 膜面渗透压 β·πf，bar（β 非法时为 0）
}

// Failure 描述阵列推进中第一段失败的位置与原因。
type Failure struct {
	// StageIndex 出问题的段号（1 起）。
	StageIndex int
	// Code 内核领域错误码，如 non_positive_ndp、non_positive_parameter。
	Code string
	// Reason 具体原因（与单段内核的拒绝口径一致）。
	Reason string
	// Feed 该段开算前的进料条件（即上一段浓水接力过来的工况）。
	Feed StageFeed
}

// GlobalSaltBalance 是跨全部段的一笔总盐账：
//
//	阵列进料盐量 = 各段产水盐量之和 + 末段浓水盐量
//
// 它与单段内部守恒相互独立：单段守恒只保证 Cf·Qf = Cp·Qp + Cb·Qb 在
// 每一段内部闭合，这里的全局账把段间接力也纳入，单独校验、单独报告。
type GlobalSaltBalance struct {
	FeedSaltMolFlow     float64 // 阵列进料盐量，mol/h
	PermeateSaltMolFlow float64 // 各段产水盐量之和 Σ Cp,k·Qp,k，mol/h
	BrineSaltMolFlow    float64 // 末段浓水盐量 Cb,n·Qb,n，mol/h
	ResidualMolFlow     float64 // 摩尔口径残差，mol/h

	// 以下质量口径（g/h）仅在工况给了摩尔质量时填写。
	FeedSaltMassFlow     float64
	PermeateSaltMassFlow float64
	BrineSaltMassFlow    float64
	ResidualMassFlow     float64
}

// Summary 是全套阵列跑通后的汇总。
type Summary struct {
	StageCount         int     // 段数
	TotalPermeateFlow  float64 // 汇总产水量 Σ Qp,k，L/h
	OverallRecovery    float64 // 汇总回收率 Σ Qp,k / Qf,1
	FinalBrineFlow     float64 // 末段浓水流量 Qb,n，L/h
	FinalBrineMolarity float64 // 末段浓水浓度 Cb,n，mol/L

	// BlendedPermeateMolarity 混合产水浓度 = Σ(Cp,k·Qp,k) / Σ Qp,k。
	// 按各段实际产水量加权，不是各段产水浓度的算术平均。
	BlendedPermeateMolarity float64

	// Salt 全局盐量守恒（独立复算并校验通过后才返回）。
	Salt GlobalSaltBalance
}

// Result 是一次阵列编排的完整返回。要么 Summary 非 nil（StatusOK），
// 要么 Failure 非 nil（StatusStageFailed），二者必居其一。
type Result struct {
	Status     string        // StatusOK / StatusStageFailed
	StageCount int           // 配置的总段数（失败时 Failure.StageIndex 指出推进到哪段停的）
	Stages     []StageResult // 已跑通的段（失败时保留失败段之前的全部结果）
	Failure    *Failure      // 第一段失败的位置与原因；跑通时为 nil
	Summary    *Summary      // 阵列汇总；仅在全部段跑通时非 nil
}
