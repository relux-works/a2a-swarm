//go:build phase1measure && !phase1cache && darwin && arm64

package config

func phase1MeasurementPaths(base Paths, identity string) (Paths, func(), func()) {
	return base, func() {}, func() {}
}

func phase1MeasurementStats() phase1MeasureStats { return phase1MeasureStats{} }
