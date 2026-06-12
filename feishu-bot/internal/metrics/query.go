package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Point struct {
	// Unix milliseconds.
	T int64 `json:"t_ms"`

	Requests int `json:"requests"`
	Failures int `json:"failures"`

	Tokens        int   `json:"tokens"`
	CostUSDMicros int64 `json:"cost_usd_micros"`
}

type QueryResult struct {
	StartUnixSec int64   `json:"start_unix_sec"`
	EndUnixSec   int64   `json:"end_unix_sec"`
	StepSec      int64   `json:"step_sec"`
	Points       []Point `json:"points"`
	Totals       Totals  `json:"totals"`
}

type Totals struct {
	Requests      int   `json:"requests"`
	Failures      int   `json:"failures"`
	Tokens        int   `json:"tokens"`
	CostUSDMicros int64 `json:"cost_usd_micros"`
}

func QueryFile(basePath string, from, to time.Time, step time.Duration, pending []MinuteSample) (QueryResult, error) {
	basePath = strings.TrimSpace(basePath)
	if basePath == "" {
		return QueryResult{}, errors.New("metrics base path is empty")
	}
	if step <= 0 {
		step = 10 * time.Minute
	}
	stepSec := int64(step.Seconds())
	if stepSec <= 0 {
		stepSec = 600
	}

	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	if toUnix <= fromUnix {
		toUnix = fromUnix + stepSec
	}

	start := (fromUnix / stepSec) * stepSec
	end := ((toUnix + stepSec - 1) / stepSec) * stepSec
	if end <= start {
		end = start + stepSec
	}

	n := int((end - start) / stepSec)
	points := make([]Point, n)
	for i := 0; i < n; i++ {
		points[i].T = (start + int64(i)*stepSec) * 1000
	}

	acc := func(s MinuteSample) {
		if s.TS < start || s.TS >= end {
			return
		}
		idx := int((s.TS - start) / stepSec)
		if idx < 0 || idx >= len(points) {
			return
		}
		p := &points[idx]
		p.Requests += s.Requests
		p.Failures += s.Failures
		p.Tokens += s.InputTokens + s.CachedInputTokens + s.OutputTokens
		p.CostUSDMicros += s.CostUSDMicros
	}

	files, err := listMetricFiles(basePath)
	if err != nil {
		// Best-effort: still include pending data even if files can't be listed.
		for _, s := range pending {
			acc(s)
		}
		return QueryResult{
			StartUnixSec: start,
			EndUnixSec:   end,
			StepSec:      stepSec,
			Points:       points,
		}, nil
	}

	for _, fp := range files {
		if err := scanMetricFile(fp, acc); err != nil {
			continue
		}
	}

	for _, s := range pending {
		acc(s)
	}

	var totals Totals
	for _, p := range points {
		totals.Requests += p.Requests
		totals.Failures += p.Failures
		totals.Tokens += p.Tokens
		totals.CostUSDMicros += p.CostUSDMicros
	}

	return QueryResult{
		StartUnixSec: start,
		EndUnixSec:   end,
		StepSec:      stepSec,
		Points:       points,
		Totals:       totals,
	}, nil
}

func scanMetricFile(path string, fn func(MinuteSample)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s MinuteSample
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		fn(s)
	}
	return sc.Err()
}

func listMetricFiles(basePath string) ([]string, error) {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// If the dir can't be listed, fall back to just the base file.
		return []string{basePath}, nil
	}

	type rotated struct {
		idx  int
		path string
	}
	var rotatedFiles []rotated

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
		rotatedFiles = append(rotatedFiles, rotated{idx: n, path: filepath.Join(dir, name)})
	}

	sort.Slice(rotatedFiles, func(i, j int) bool { return rotatedFiles[i].idx < rotatedFiles[j].idx })

	out := make([]string, 0, len(rotatedFiles)+1)
	for _, r := range rotatedFiles {
		out = append(out, r.path)
	}
	out = append(out, basePath)

	return out, nil
}
