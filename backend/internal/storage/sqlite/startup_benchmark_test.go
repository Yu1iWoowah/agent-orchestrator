package sqlite

import "testing"

// BenchmarkSQLiteStartup separates an empty database's full migration from the
// production reopen path. Both include closing the pools; directory setup is
// outside the timer. Run fixed iterations on alternating builds for comparison.
func BenchmarkSQLiteStartup(b *testing.B) {
	for _, mode := range []string{"fresh", "reopen"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			var dataDir string
			if mode == "reopen" {
				dataDir = b.TempDir()
				store, err := Open(dataDir)
				if err != nil {
					b.Fatal(err)
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for range b.N {
				if mode == "fresh" {
					dataDir = b.TempDir()
				}
				b.StartTimer()
				store, err := Open(dataDir)
				if err != nil {
					b.Fatal(err)
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
			}
			b.ReportMetric(float64(b.N), "completed-opens")
		})
	}
}
