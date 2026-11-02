package persistenthost

import (
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func BenchmarkDeliveryNativeProgress(b *testing.B) {
	j, err := newDeliveryJournal(filepath.Join(b.TempDir(), "delivery.journal"), "epoch")
	if err != nil {
		b.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":1,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"benchmark-attempt"}}}`)); err != nil {
		b.Fatal(err)
	}
	if err = j.observe([]byte(`{"id":1,"result":{"turn":{"id":"native-turn"}}}`)); err != nil {
		b.Fatal(err)
	}
	before, err := j.file.Stat()
	if err != nil {
		b.Fatal(err)
	}
	start := []byte(`{"method":"item/started","params":{"turnId":"native-turn","item":{"id":"tool","type":"commandExecution"}}}`)
	done := []byte(`{"method":"item/completed","params":{"turnId":"native-turn","item":{"id":"tool","type":"commandExecution"}}}`)
	b.ResetTimer()
	for range b.N {
		if err = j.observe(start); err != nil {
			b.Fatal(err)
		}
		if err = j.observe(done); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	after, err := j.file.Stat()
	if err != nil {
		b.Fatal(err)
	}
	record := j.records["benchmark-attempt"]
	if record.Receipt.Disposition != domain.ChatDeliveryAccepted || record.Receipt.Progress != "running" {
		b.Fatalf("native progress not processed: %+v", record.Receipt)
	}
	b.ReportMetric(float64(after.Size()-before.Size())/float64(2*b.N), "journal-B/frame")
	b.ReportMetric(float64(2*b.N), "frames")
}
