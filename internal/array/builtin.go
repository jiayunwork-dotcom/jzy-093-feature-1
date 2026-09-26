package array

import (
	"rocalc/internal/osmotic"
)

// BuiltinTrainName 是内置三段串联阵列配置的名称。
const BuiltinTrainName = "brackish-train-3"

// BuiltinTrain 内置的三段串联苦咸水阵列（与内置工况档同一份进料）：
//
// 进料 5 g/L NaCl（M=58.44 g/mol），25 ℃，i=2，Qf=1000 L/h；
// 三段膜面积递减（36 / 20 / 12 m²），工作压差逐段微升（12 / 13 / 13.5 bar），
// Lp=1.8 LMH/bar，r=0.98（部分截留，产水带盐），β=1。
//
// 前段浓水浓度抬高后渗透压随之上升，压差逐段加大才能维持正 NDP，
// 这正是多段阵列编排要处理的典型工况。
func BuiltinTrain() Config {
	return Config{
		Name: BuiltinTrainName,
		Feed: osmotic.Solution{
			MassConcentration: 5.0,
			MolarMass:         58.44,
			Temperature:       298.15,
			VanTHoff:          2,
		},
		FeedFlow: 1000,
		Stages: []StageSpec{
			{Area: 36, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 12},
			{Area: 20, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 13},
			{Area: 12, Permeability: 1.8, Rejection: 0.98, Polarization: 1, AppliedPressure: 13.5},
		},
	}
}

// NewDefaultRegistry 建表并登记内置三段串联配置。
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Put(BuiltinTrain())
	return r
}
