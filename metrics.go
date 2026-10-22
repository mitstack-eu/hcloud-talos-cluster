package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	seriesCPU       = "cpu"
	seriesDiskRead  = "disk.0.bandwidth.read"
	seriesDiskWrite = "disk.0.bandwidth.write"
	// busyCPUPercent sits above an idle server's CPU and far below what a docker pull or imager takes.
	busyCPUPercent = 10
)

// builderMetrics is what Hetzner's metrics read for a builder since its creation, so a stalled build shows where it
// stopped; Hetzner's network series stay empty for a server with only a private network, so it reads CPU and disk alone.
type builderMetrics struct {
	read bool
	hcMetrics
}

// readBuilderMetrics never fails its caller, since a builder's metrics only explain a build.
func readBuilderMetrics(ctx context.Context, api hetznerAPI, builder hcServer) builderMetrics {
	m, err := api.ServerMetrics(ctx, builder.ID, builder.Created, clock())
	return builderMetrics{read: err == nil, hcMetrics: m}
}

func (m builderMetrics) summary() string {
	cpu, writes := m.Series[seriesCPU], m.Series[seriesDiskWrite]
	switch {
	case !m.read:
		return "the builder's Hetzner metrics could not be read"
	case len(cpu) == 0 && len(writes) == 0:
		return "Hetzner holds no metrics for the builder yet"
	}
	var written float64
	for _, p := range writes {
		written += p.Value * m.Step.Seconds()
	}
	return fmt.Sprintf("Hetzner reads its CPU at %.1f%% over the last minute, %s written to disk over the last minute and %s since its creation",
		lastValue(cpu), formatBytes(lastValue(writes)*time.Minute.Seconds()), formatBytes(written))
}

// stall adds to the summary when the disk last moved and when CPU was last busy.
func (m builderMetrics) stall() string {
	if !m.read || len(m.Series[seriesCPU])+len(m.Series[seriesDiskWrite]) == 0 {
		return m.summary()
	}
	disk := "its disk never moved"
	read, readMoved := lastAbove(m.Series[seriesDiskRead], 0)
	write, writeMoved := lastAbove(m.Series[seriesDiskWrite], 0)
	if readMoved || writeMoved {
		if write.After(read) {
			read = write
		}
		disk = "its disk last moved at " + read.UTC().Format(time.RFC3339)
	}
	cpu := fmt.Sprintf("its CPU was never above %d%%", busyCPUPercent)
	if at, ok := lastAbove(m.Series[seriesCPU], busyCPUPercent); ok {
		cpu = fmt.Sprintf("its CPU was last above %d%% at %s", busyCPUPercent, at.UTC().Format(time.RFC3339))
	}
	return strings.Join([]string{m.summary(), disk, cpu}, "; ")
}

func lastValue(points []metricPoint) float64 {
	if len(points) == 0 {
		return 0
	}
	return points[len(points)-1].Value
}

func lastAbove(points []metricPoint, floor float64) (time.Time, bool) {
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].Value > floor {
			return points[i].At, true
		}
	}
	return time.Time{}, false
}

func formatBytes(n float64) string {
	if n < 1024 {
		return fmt.Sprintf("%.0f B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
