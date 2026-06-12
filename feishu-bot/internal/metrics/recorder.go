package metrics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"example.com/feishu-bot/internal/codex"
)

type Pricing struct {
	// USD per 1,000,000 tokens.
	//
	// If TotalUSDPer1M > 0, we compute cost using:
	//   cost = (input + cached_input + output) * TotalUSDPer1M / 1e6
	// (implemented in micro-USD as: cost_usd_micros = total_tokens * TotalUSDPer1M).
	TotalUSDPer1M float64

	InputUSDPer1M       float64
	CachedInputUSDPer1M float64
	OutputUSDPer1M      float64
}

func (p Pricing) CostUSDMicros(u codex.Usage) int64 {
	// Cost in micro-USD (1e-6 USD). This keeps numbers stable and avoids float
	// drift when aggregating over time.
	//
	// If the caller provides the OpenAI USD-per-1M price, then:
	//   cost_usd = tokens/1e6 * price_usd_per_1m
	//   cost_usd_micros = cost_usd * 1e6 = tokens * price_usd_per_1m
	if p.TotalUSDPer1M > 0 {
		return int64(math.Round(float64(u.TotalTokens()) * p.TotalUSDPer1M))
	}

	cost := 0.0
	if p.InputUSDPer1M > 0 && u.InputTokens > 0 {
		cost += float64(u.InputTokens) * p.InputUSDPer1M
	}
	if p.CachedInputUSDPer1M > 0 && u.CachedInputTokens > 0 {
		cost += float64(u.CachedInputTokens) * p.CachedInputUSDPer1M
	}
	if p.OutputUSDPer1M > 0 && u.OutputTokens > 0 {
		cost += float64(u.OutputTokens) * p.OutputUSDPer1M
	}
	return int64(math.Round(cost))
}

type MinuteSample struct {
	// Unix seconds for the start of the minute (UTC).
	TS int64 `json:"ts"`

	Requests int `json:"requests"`
	Failures int `json:"failures"`

	InputTokens       int   `json:"input_tokens"`
	CachedInputTokens int   `json:"cached_input_tokens"`
	OutputTokens      int   `json:"output_tokens"`
	CostUSDMicros     int64 `json:"cost_usd_micros"`
}

type Recorder struct {
	filePath       string
	rotateMaxBytes int64
	flushInterval  time.Duration
	pricing        Pricing

	mu       sync.Mutex
	pending  map[int64]*MinuteSample // key: minute start unix seconds
	inFlight atomic.Int64

	f  *os.File
	bw *bufio.Writer
}

type Options struct {
	FilePath       string
	RotateMaxBytes int64
	FlushInterval  time.Duration
	Pricing        Pricing
}

func NewRecorder(opts Options) (*Recorder, error) {
	fp := strings.TrimSpace(opts.FilePath)
	if fp == "" {
		return nil, errors.New("metrics file path is empty")
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Minute
	}
	if opts.RotateMaxBytes <= 0 {
		opts.RotateMaxBytes = 50 * 1024 * 1024
	}

	dir := filepath.Dir(fp)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir metrics dir: %w", err)
		}
	}

	return &Recorder{
		filePath:       fp,
		rotateMaxBytes: opts.RotateMaxBytes,
		flushInterval:  opts.FlushInterval,
		pricing:        opts.Pricing,
		pending:        make(map[int64]*MinuteSample),
	}, nil
}

func (r *Recorder) Add(ts time.Time, success bool, usage codex.Usage) {
	if r == nil {
		return
	}
	minTS := ts.UTC().Truncate(time.Minute).Unix()

	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.pending[minTS]
	if !ok {
		s = &MinuteSample{TS: minTS}
		r.pending[minTS] = s
	}

	s.Requests++
	if !success {
		s.Failures++
	}
	s.InputTokens += usage.InputTokens
	s.CachedInputTokens += usage.CachedInputTokens
	s.OutputTokens += usage.OutputTokens
	s.CostUSDMicros += r.pricing.CostUSDMicros(usage)
}

func (r *Recorder) IncInFlight() int64 {
	if r == nil {
		return 0
	}
	return r.inFlight.Add(1)
}

func (r *Recorder) DecInFlight() int64 {
	if r == nil {
		return 0
	}
	return r.inFlight.Add(-1)
}

func (r *Recorder) InFlight() int64 {
	if r == nil {
		return 0
	}
	return r.inFlight.Load()
}

func (r *Recorder) SnapshotPending() []MinuteSample {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]MinuteSample, 0, len(r.pending))
	for _, s := range r.pending {
		if s == nil {
			continue
		}
		out = append(out, *s)
	}
	return out
}

func (r *Recorder) Run(ctx context.Context) {
	if r == nil {
		return
	}

	// Align flushes to the next interval boundary (usually the next minute).
	for {
		now := time.Now()
		next := now.UTC().Truncate(r.flushInterval).Add(r.flushInterval)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = r.FlushAll(time.Now())
			_ = r.Close()
			return
		case <-timer.C:
			timer.Stop()
			_ = r.Flush(time.Now())
		}
	}
}

func (r *Recorder) Flush(now time.Time) error {
	if r == nil {
		return nil
	}

	cutoff := now.UTC().Truncate(time.Minute).Unix()

	r.mu.Lock()
	defer r.mu.Unlock()

	keys := make([]int64, 0, len(r.pending))
	for k := range r.pending {
		// Only flush completed minutes. Keep the current (in-progress) minute.
		if k < cutoff {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	if err := r.ensureFileLocked(); err != nil {
		return err
	}

	for _, k := range keys {
		s := r.pending[k]
		if s == nil {
			delete(r.pending, k)
			continue
		}
		// Emit one JSON record per minute.
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if _, err := r.bw.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("write metrics: %w", err)
		}
		delete(r.pending, k)
	}

	if err := r.bw.Flush(); err != nil {
		return fmt.Errorf("flush metrics: %w", err)
	}
	_ = r.f.Sync()

	if err := r.rotateIfNeededLocked(); err != nil {
		return err
	}
	return nil
}

func (r *Recorder) FlushAll(now time.Time) error {
	if r == nil {
		return nil
	}
	// Flush everything up to "now" (including the current minute).
	cutoff := now.UTC().Truncate(time.Minute).Add(time.Minute).Unix()

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.pending) == 0 {
		return nil
	}
	keys := make([]int64, 0, len(r.pending))
	for k := range r.pending {
		if k < cutoff {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	if err := r.ensureFileLocked(); err != nil {
		return err
	}

	for _, k := range keys {
		s := r.pending[k]
		if s == nil {
			delete(r.pending, k)
			continue
		}
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if _, err := r.bw.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("write metrics: %w", err)
		}
		delete(r.pending, k)
	}

	if err := r.bw.Flush(); err != nil {
		return fmt.Errorf("flush metrics: %w", err)
	}
	_ = r.f.Sync()

	if err := r.rotateIfNeededLocked(); err != nil {
		return err
	}
	return nil
}

func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.bw != nil {
		_ = r.bw.Flush()
	}
	if r.f != nil {
		_ = r.f.Sync()
		_ = r.f.Close()
	}
	r.bw = nil
	r.f = nil
	return nil
}

func (r *Recorder) ensureFileLocked() error {
	if r.f != nil && r.bw != nil {
		return nil
	}

	f, err := os.OpenFile(r.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open metrics file: %w", err)
	}
	r.f = f
	r.bw = bufio.NewWriterSize(f, 64*1024)
	return nil
}

func (r *Recorder) rotateIfNeededLocked() error {
	if r == nil || r.f == nil {
		return nil
	}
	if r.rotateMaxBytes <= 0 {
		return nil
	}
	st, err := r.f.Stat()
	if err != nil {
		return nil
	}
	if st.Size() <= r.rotateMaxBytes {
		return nil
	}

	// Close current file before renaming.
	_ = r.bw.Flush()
	_ = r.f.Sync()
	_ = r.f.Close()
	r.bw = nil
	r.f = nil

	rotated, err := nextRotatePath(r.filePath)
	if err != nil {
		// Best-effort: if we can't determine the rotated name, reopen and keep writing.
		_ = r.ensureFileLocked()
		return nil
	}

	if err := os.Rename(r.filePath, rotated); err != nil {
		_ = r.ensureFileLocked()
		return nil
	}

	return r.ensureFileLocked()
}

func nextRotatePath(basePath string) (string, error) {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	maxIdx := -1
	prefix := base + "."
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suf := strings.TrimPrefix(name, prefix)
		n, err := strconv.Atoi(suf)
		if err != nil {
			continue
		}
		if n > maxIdx {
			maxIdx = n
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s.%d", base, maxIdx+1)), nil
}
