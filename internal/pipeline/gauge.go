package pipeline

import "github.com/prometheus/client_golang/prometheus"

func gaugeFunc(name, help string, f func() float64) prometheus.GaugeFunc {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f)
}
