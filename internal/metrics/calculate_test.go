package metrics_test

import (
	"math"
	"testing"
	"time"

	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/protocol"
)

// sampleOf builds a sample of a container at a point in time. The counters
// grow with the time, so two samples of the helper describe an interval.
func sampleOf(containerID string, at time.Time) protocol.RawSample {
	seconds := uint64(at.Unix())
	return protocol.RawSample{
		ContainerID:   containerID,
		CollectedAt:   at,
		CPUUsageNS:    seconds * 1_000,
		SystemUsageNS: seconds * 10_000,
		OnlineCPUs:    2,
		MemoryUsage:   512 * 1024 * 1024,
		MemoryLimit:   1024 * 1024 * 1024,
		NetRX:         seconds * 100,
		NetTX:         seconds * 200,
		IORead:        seconds * 300,
		IOWrite:       seconds * 400,
	}
}

// TestCounterResetCreatesGapInsteadOfNegativeRate is the test of the brief.
func TestCounterResetCreatesGapInsteadOfNegativeRate(t *testing.T) {
	previous := protocol.RawSample{ContainerID: "c1", CollectedAt: time.Unix(100, 0), NetRX: 1000, CPUUsageNS: 100, SystemUsageNS: 1000, OnlineCPUs: 2}
	current := protocol.RawSample{ContainerID: "c1", CollectedAt: time.Unix(110, 0), NetRX: 10, CPUUsageNS: 200, SystemUsageNS: 2000, OnlineCPUs: 2}
	result := metrics.Calculate(previous, current)
	if result.Valid {
		t.Fatalf("counter reset accepted: %+v", result)
	}
}

// TestTheCPUFluctuatesWithEachCoreAtOneHundredPercent proves that the CPU is
// measured per core: a container that uses two cores of four reports 200 per
// cent and not 50 per cent.
func TestTheCPUFluctuatesWithEachCoreAtOneHundredPercent(t *testing.T) {
	previous := sampleOf("c1", time.Unix(100, 0))
	current := sampleOf("c1", time.Unix(110, 0))
	current.CPUUsageNS = previous.CPUUsageNS + 1_000
	current.SystemUsageNS = previous.SystemUsageNS + 2_000
	current.OnlineCPUs = 4

	result := metrics.Calculate(previous, current)
	if !result.Valid {
		t.Fatalf("the sample is not valid: %+v", result)
	}
	if want := 200.0; !almostEqual(result.Values.CPUPercent, want) {
		t.Fatalf("the CPU = %v, want %v", result.Values.CPUPercent, want)
	}
}

// TestTheRatesUseTheSameTimeBaseAndBytes proves the units of the rates:
// bytes per second over the time between the two samples.
func TestTheRatesUseTheSameTimeBaseAndBytes(t *testing.T) {
	previous := sampleOf("c1", time.Unix(100, 0))
	current := sampleOf("c1", time.Unix(110, 0))
	current.NetRX = previous.NetRX + 2_000
	current.NetTX = previous.NetTX + 5_000
	current.IORead = previous.IORead + 10_000
	current.IOWrite = previous.IOWrite + 20_000

	result := metrics.Calculate(previous, current)
	if !result.Valid {
		t.Fatalf("the sample is not valid: %+v", result)
	}
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"net receive", result.Values.NetRXPerSec, 200},
		{"net transmit", result.Values.NetTXPerSec, 500},
		{"io read", result.Values.IOReadPerSec, 1_000},
		{"io write", result.Values.IOWritePerSec, 2_000},
	}
	for _, c := range cases {
		if !almostEqual(c.got, c.want) {
			t.Fatalf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestTheMemoryIsTheUsageOfTheSample proves that the memory is the current
// usage in bytes and not a rate and not the limit.
func TestTheMemoryIsTheUsageOfTheSample(t *testing.T) {
	previous := sampleOf("c1", time.Unix(100, 0))
	current := sampleOf("c1", time.Unix(110, 0))
	current.MemoryUsage = 700 * 1024 * 1024
	current.MemoryLimit = 0

	result := metrics.Calculate(previous, current)
	if !result.Valid {
		t.Fatalf("the sample is not valid: %+v", result)
	}
	if want := float64(700 * 1024 * 1024); result.Values.MemoryBytes != want {
		t.Fatalf("the memory = %v, want %v", result.Values.MemoryBytes, want)
	}
}

// TestAnUnlimitedContainerDoesNotClaimZeroBytes proves that a container
// without an explicit limit keeps the zero limit of the raw sample: the
// metrics never turn an unlimited container into one with a zero byte limit.
func TestAnUnlimitedContainerDoesNotClaimZeroBytes(t *testing.T) {
	previous := sampleOf("c1", time.Unix(100, 0))
	current := sampleOf("c1", time.Unix(110, 0))
	current.MemoryLimit = 0

	result := metrics.Calculate(previous, current)
	if !result.Valid {
		t.Fatalf("the sample is not valid: %+v", result)
	}
	if current.MemoryLimit != 0 {
		t.Fatalf("the limit = %d, want 0 for an unlimited container", current.MemoryLimit)
	}
	if result.Values.MemoryBytes == 0 {
		t.Fatal("the metrics reported zero usage for a running container")
	}
}

// TestAnInvalidSampleIsNotReportedAsZero proves that a gap is a gap: the
// sample is marked invalid instead of being turned into a valid zero rate.
func TestAnInvalidSampleIsNotReportedAsZero(t *testing.T) {
	previous := sampleOf("c1", time.Unix(100, 0))
	current := sampleOf("c1", time.Unix(110, 0))

	cases := []struct {
		name     string
		previous protocol.RawSample
		current  protocol.RawSample
	}{
		{
			name:     "first sample",
			previous: protocol.RawSample{},
			current:  current,
		},
		{
			name:     "other container",
			previous: previous,
			current:  sampleOf("c2", time.Unix(110, 0)),
		},
		{
			name:     "the time does not move",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.CollectedAt = previous.CollectedAt
				return next
			}(),
		},
		{
			name:     "the time moves backwards",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.CollectedAt = previous.CollectedAt.Add(-time.Second)
				return next
			}(),
		},
		{
			name:     "the CPU counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.CPUUsageNS = previous.CPUUsageNS - 1
				return next
			}(),
		},
		{
			name:     "the system counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.SystemUsageNS = previous.SystemUsageNS - 1
				return next
			}(),
		},
		{
			name:     "the receive counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.NetRX = previous.NetRX - 1
				return next
			}(),
		},
		{
			name:     "the transmit counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.NetTX = previous.NetTX - 1
				return next
			}(),
		},
		{
			name:     "the read counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.IORead = previous.IORead - 1
				return next
			}(),
		},
		{
			name:     "the write counter drops",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.IOWrite = previous.IOWrite - 1
				return next
			}(),
		},
		{
			name:     "the system time does not move",
			previous: previous,
			current: func() protocol.RawSample {
				next := current
				next.SystemUsageNS = previous.SystemUsageNS
				return next
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := metrics.Calculate(c.previous, c.current)
			if result.Valid {
				t.Fatalf("the invalid sample was accepted: %+v", result)
			}
			if result.Values != (metrics.Values{}) {
				t.Fatalf("the invalid sample became a zero rate: %+v", result.Values)
			}
		})
	}
}

// almostEqual compares two rates with a small tolerance.
func almostEqual(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}
