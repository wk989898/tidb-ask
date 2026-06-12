package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/feishu-bot/internal/codex"
)

func TestPricingCostUSDMicros(t *testing.T) {
	p := Pricing{
		InputUSDPer1M:       5.0,
		CachedInputUSDPer1M: 2.5,
		OutputUSDPer1M:      15.0,
	}
	u := codex.Usage{
		InputTokens:       100,
		CachedInputTokens: 200,
		OutputTokens:      300,
	}
	got := p.CostUSDMicros(u)
	// cost_usd_micros = tokens * price_usd_per_1m
	// = 100*5 + 200*2.5 + 300*15 = 500 + 500 + 4500 = 5500
	if got != 5500 {
		t.Fatalf("unexpected cost: got=%d want=%d", got, 5500)
	}
}

func TestRecorderRotate(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "metrics.dat")

	r, err := NewRecorder(Options{
		FilePath:       base,
		RotateMaxBytes: 200, // tiny to force rotate
		FlushInterval:  time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	// Add enough distinct minutes so we produce multiple JSON lines.
	for i := 0; i < 10; i++ {
		ts := time.Unix(int64(i*60), 0)
		r.Add(ts, true, codex.Usage{InputTokens: 1, OutputTokens: 1})
	}

	if err := r.FlushAll(time.Unix(1000, 0)); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(base + ".0"); err != nil {
		t.Fatalf("expected rotated file %q to exist: %v", base+".0", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("expected new base file %q to exist after rotate: %v", base, err)
	}
}

func TestQueryFileAggregatesBuckets(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "metrics.dat")

	// Two minute samples within the same 10-minute bucket [0,600).
	data := `{"ts":0,"requests":1,"failures":0,"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"cost_usd_micros":100}
{"ts":60,"requests":2,"failures":1,"input_tokens":3,"cached_input_tokens":2,"output_tokens":1,"cost_usd_micros":50}
`
	if err := os.WriteFile(base, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	from := time.Unix(0, 0)
	to := time.Unix(600, 0)
	res, err := QueryFile(base, from, to, 10*time.Minute, nil)
	if err != nil {
		t.Fatalf("QueryFile: %v", err)
	}
	if len(res.Points) != 1 {
		t.Fatalf("unexpected points length: got=%d want=%d", len(res.Points), 1)
	}
	p := res.Points[0]
	if p.Requests != 3 {
		t.Fatalf("requests: got=%d want=%d", p.Requests, 3)
	}
	if p.Failures != 1 {
		t.Fatalf("failures: got=%d want=%d", p.Failures, 1)
	}
	// tokens = (10+0+5) + (3+2+1) = 21
	if p.Tokens != 21 {
		t.Fatalf("tokens: got=%d want=%d", p.Tokens, 21)
	}
	if p.CostUSDMicros != 150 {
		t.Fatalf("cost_usd_micros: got=%d want=%d", p.CostUSDMicros, 150)
	}

	if res.Totals.Requests != 3 || res.Totals.Failures != 1 || res.Totals.Tokens != 21 || res.Totals.CostUSDMicros != 150 {
		t.Fatalf("unexpected totals: %+v", res.Totals)
	}
}
