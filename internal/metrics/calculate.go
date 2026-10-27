// Package metrics turns the raw samples of a container into the values the
// platform shows and stores.
//
// The package is pure calculation: it never talks to Docker and never
// writes a database. The sampling belongs to the node, the transport to the
// connection and the storage to the tasks of the history.
package metrics

import "hmoob-dock/internal/protocol"

// Values are the rates and the usage of a container at one point in time.
//
// The CPU follows the convention of one hundred percent per core, so a
// container that uses several cores may exceed one hundred percent. The
// memory is the current usage of Docker in bytes. The network and the block
// device are rates in bytes per second.
type Values struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryBytes   float64 `json:"memory_bytes"`
	NetRXPerSec   float64 `json:"net_rx_per_sec"`
	NetTXPerSec   float64 `json:"net_tx_per_sec"`
	IOReadPerSec  float64 `json:"io_read_per_sec"`
	IOWritePerSec float64 `json:"io_write_per_sec"`
}

// Derived is the result of the calculation of two samples.
//
// Valid reports whether the two samples describe the same running container
// over a positive interval with counters that did not go backwards. An
// invalid result is a gap of the observation and not a zero rate: a reader
// must not show it as an idle container.
type Derived struct {
	// Values are the calculated values. They are zero while Valid is
	// false.
	Values Values
	// Valid reports whether the values describe an observed interval.
	Valid bool
}

// Calculate derives the values of a container from two consecutive raw
// samples.
//
// The first sample of a container has no predecessor and is a gap. A
// container that was replaced, a time that did not move, a counter that
// went backwards and a system counter that did not move are gaps as well:
// the subtraction would underflow or divide by zero, so the result is
// marked invalid instead of being turned into a rate of zero.
func Calculate(previous, current protocol.RawSample) Derived {
	if previous.ContainerID == "" || previous.CollectedAt.IsZero() {
		// The first sample of a container has no interval yet.
		return Derived{}
	}
	if current.ContainerID == "" || current.ContainerID != previous.ContainerID {
		// The counters of another container do not continue the first
		// ones.
		return Derived{}
	}
	if current.CollectedAt.IsZero() || !current.CollectedAt.After(previous.CollectedAt) {
		// A time that did not move cannot be the base of a rate.
		return Derived{}
	}
	if current.CPUUsageNS < previous.CPUUsageNS ||
		current.SystemUsageNS < previous.SystemUsageNS ||
		current.NetRX < previous.NetRX ||
		current.NetTX < previous.NetTX ||
		current.IORead < previous.IORead ||
		current.IOWrite < previous.IOWrite {
		// A counter that went backwards was reset. The difference would
		// underflow, so the sample is a gap.
		return Derived{}
	}

	systemDelta := current.SystemUsageNS - previous.SystemUsageNS
	if systemDelta == 0 {
		// The host did not spend CPU time between the samples, so the
		// ratio of the CPU is undefined.
		return Derived{}
	}

	elapsed := current.CollectedAt.Sub(previous.CollectedAt).Seconds()
	cpu := float64(current.CPUUsageNS-previous.CPUUsageNS) /
		float64(systemDelta) * float64(current.OnlineCPUs) * 100

	return Derived{
		Valid: true,
		Values: Values{
			CPUPercent:    cpu,
			MemoryBytes:   float64(current.MemoryUsage),
			NetRXPerSec:   float64(current.NetRX-previous.NetRX) / elapsed,
			NetTXPerSec:   float64(current.NetTX-previous.NetTX) / elapsed,
			IOReadPerSec:  float64(current.IORead-previous.IORead) / elapsed,
			IOWritePerSec: float64(current.IOWrite-previous.IOWrite) / elapsed,
		},
	}
}
