//go:build !linux

package controlplane

import "runtime"

func runtimeCPUCapacity(_ bool) float64 { return float64(runtime.NumCPU()) }
