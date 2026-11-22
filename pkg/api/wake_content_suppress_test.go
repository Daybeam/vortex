package api

import (
	"testing"
)

func TestSigDigits(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"0.552", 3},
		{"0.096", 2},
		{"0.3", 1},
		{"0.2", 1},
		{"575.0", 4},
		{"690.0", 4},
		{"125.0", 4},
		{"700", 1},
		{"3660", 3},
		{"2391.2", 5},
		{"3108.56", 6},
		{"1268.8", 5},
		{"0", 0},
		{"1", 1},
		{"2", 1},
		{"3", 1},
		{"-0.552", 3},
		{"+575.0", 4},
		{"100", 1},
		{"100.0", 4},
		{"0.001", 1},
		{"0.010", 2},
	}
	for _, c := range cases {
		got := sigDigits(c.s)
		if got != c.want {
			t.Errorf("sigDigits(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestExtractJSONFigures(t *testing.T) {
	t.Run("root scalars only", func(t *testing.T) {
		data := []byte(`{"total_hourly_cost_usd": 0.552, "name": "test", "count": 3}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"3", "0.552"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("one dict layer deep", func(t *testing.T) {
		data := []byte(`{
			"total": 0.552,
			"service": {"hourly_cost": 0.456, "on_demand_count": 3}
		}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"0.456", "3", "0.552"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("two dict layers excluded", func(t *testing.T) {
		data := []byte(`{
			"total": 0.552,
			"service": {
				"cost": 0.456,
				"zone_counts": {"us_east": 575.0, "us_west": 690.0}
			}
		}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"0.456", "0.552"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v (zone_counts at dict depth 2 should be excluded)", figs, want)
		}
	})

	t.Run("arrays do not count as dict layer", func(t *testing.T) {
		data := []byte(`{
			"total_hourly_cost_usd": 0.552,
			"services": [
				{"name": "checkout-api", "hourly_cost": 0.456, "forecast_peak": 3660},
				{"name": "pricing-worker", "hourly_cost": 0.096, "forecast_peak": 2391.2}
			]
		}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"3660", "0.456", "2391.2", "0.096", "0.552"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("array of dicts with nested dict excluded", func(t *testing.T) {
		data := []byte(`{
			"services": [
				{"cost": 0.456, "zone_counts": {"a": 575.0}}
			]
		}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"0.456"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v (zone_counts inside array element is at dict depth 2)", figs, want)
		}
	})

	t.Run("minSig filters low-significance values", func(t *testing.T) {
		data := []byte(`{
			"total": 0.552,
			"min_zones": 2,
			"required_headroom_pct": 0.3,
			"forecast_peak": 2391.2
		}`)
		figs := extractJSONFigures(data, 4)
		want := []string{"2391.2"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v (only 2391.2 has >= 4 sig digits)", figs, want)
		}
	})

	t.Run("trailing zeros with decimal point count", func(t *testing.T) {
		data := []byte(`{"a": 575.0, "b": 690.0, "c": 700}`)
		figs := extractJSONFigures(data, 4)
		want := []string{"575", "690"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v (575.0 and 690.0 have 4 sig digits with decimal, 700 has 1)", figs, want)
		}
	})

	t.Run("scalar array elements at root depth", func(t *testing.T) {
		data := []byte(`{"values": [1.5, 2.3, 3.7]}`)
		figs := extractJSONFigures(data, 1)
		want := []string{"1.5", "2.3", "3.7"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		data := []byte(`not json`)
		figs := extractJSONFigures(data, 1)
		if figs != nil {
			t.Errorf("got %v, want nil", figs)
		}
	})

	t.Run("JSON array at root returns nil", func(t *testing.T) {
		data := []byte(`[1, 2, 3]`)
		figs := extractJSONFigures(data, 1)
		if figs != nil {
			t.Errorf("got %v, want nil (root must be object)", figs)
		}
	})
}

func TestHeadlineFigures(t *testing.T) {
	dir := t.TempDir()

	t.Run("reads declared JSON artifacts", func(t *testing.T) {
		writeFile(t, dir, "plan.json", `{
			"total_hourly_cost_usd": 0.552,
			"services": [
				{"name": "checkout-api", "hourly_cost": 0.456, "forecast_peak": 3660},
				{"name": "pricing-worker", "hourly_cost": 0.096, "forecast_peak": 2391.2}
			]
		}`)
		figs := headlineFigures(dir, []string{"plan.json"}, 3)
		want := []string{"3660", "0.456", "2391.2", "0.552"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("minSig=4 filters to high-significance only", func(t *testing.T) {
		writeFile(t, dir, "plan2.json", `{
			"total_hourly_cost_usd": 0.552,
			"forecast_peak": 2391.2,
			"required_capacity": 3108.56,
			"min_zones": 2
		}`)
		figs := headlineFigures(dir, []string{"plan2.json"}, 4)
		want := []string{"2391.2", "3108.56"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v", figs, want)
		}
	})

	t.Run("deduplicates across files", func(t *testing.T) {
		writeFile(t, dir, "a.json", `{"cost": 0.552}`)
		writeFile(t, dir, "b.json", `{"price": 0.552, "other": 1.5}`)
		figs := headlineFigures(dir, []string{"a.json", "b.json"}, 1)
		want := []string{"0.552", "1.5"}
		if !equalStrings(figs, want) {
			t.Errorf("got %v, want %v (0.552 should appear once)", figs, want)
		}
	})

	t.Run("missing file skipped", func(t *testing.T) {
		figs := headlineFigures(dir, []string{"nonexistent.json"}, 1)
		if len(figs) != 0 {
			t.Errorf("got %v, want empty", figs)
		}
	})

	t.Run("non-JSON file skipped", func(t *testing.T) {
		writeFile(t, dir, "data.csv", "name,cost\nfoo,0.552\n")
		figs := headlineFigures(dir, []string{"data.csv"}, 1)
		if len(figs) != 0 {
			t.Errorf("got %v, want empty (CSV not supported yet)", figs)
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		figs := headlineFigures(dir, []string{"../../../etc/passwd"}, 1)
		if len(figs) != 0 {
			t.Errorf("got %v, want empty (path traversal should be rejected)", figs)
		}
	})

	t.Run("empty declared returns nil", func(t *testing.T) {
		figs := headlineFigures(dir, nil, 1)
		if figs != nil {
			t.Errorf("got %v, want nil", figs)
		}
	})

	t.Run("empty root returns nil", func(t *testing.T) {
		figs := headlineFigures("", []string{"plan.json"}, 1)
		if figs != nil {
			t.Errorf("got %v, want nil", figs)
		}
	})
}

func TestFigureHits(t *testing.T) {
	t.Run("empty figures returns nil", func(t *testing.T) {
		hits := figureHits("the answer is 0.552", nil)
		if hits != nil {
			t.Errorf("got %v, want nil", hits)
		}
	})

	t.Run("empty answer returns nil", func(t *testing.T) {
		hits := figureHits("", []string{"0.552"})
		if hits != nil {
			t.Errorf("got %v, want nil", hits)
		}
	})

	t.Run("exact match", func(t *testing.T) {
		hits := figureHits("the total cost is 0.552 per hour", []string{"0.552"})
		want := []string{"0.552"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})

	t.Run("no match", func(t *testing.T) {
		hits := figureHits("the task completed successfully", []string{"0.552"})
		if len(hits) != 0 {
			t.Errorf("got %v, want empty", hits)
		}
	})

	t.Run("string prefix does not match", func(t *testing.T) {
		hits := figureHits("the value is 0.55", []string{"0.552"})
		if len(hits) != 0 {
			t.Errorf("got %v, want empty (0.55 should not match 0.552)", hits)
		}
	})

	t.Run("currency symbols stripped", func(t *testing.T) {
		hits := figureHits("the cost is $0.552/hour", []string{"0.552"})
		want := []string{"0.552"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})

	t.Run("percent signs stripped", func(t *testing.T) {
		hits := figureHits("headroom is 30%", []string{"30"})
		want := []string{"30"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})

	t.Run("thousands separators stripped", func(t *testing.T) {
		hits := figureHits("peak was 3,660 requests", []string{"3660"})
		want := []string{"3660"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})

	t.Run("multiple figures multiple hits", func(t *testing.T) {
		hits := figureHits(
			"total cost 0.552, peak 3660, capacity 2391.2",
			[]string{"0.552", "3660", "2391.2", "690"},
		)
		want := []string{"0.552", "3660", "2391.2"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})

	t.Run("negative numbers", func(t *testing.T) {
		hits := figureHits("delta was -15.3 units", []string{"-15.3"})
		want := []string{"-15.3"}
		if !equalStrings(hits, want) {
			t.Errorf("got %v, want %v", hits, want)
		}
	})
}

func TestWakeContentSuppressSetting(t *testing.T) {
	cases := []struct {
		env  string
		want wakeContentSuppressMode
	}{
		{"", wakeContentSuppressOff},
		{"off", wakeContentSuppressOff},
		{"OFF", wakeContentSuppressOff},
		{"observe", wakeContentSuppressObserve},
		{"OBSERVE", wakeContentSuppressObserve},
		{"log", wakeContentSuppressObserve},
		{"on", wakeContentSuppressOn},
		{"ON", wakeContentSuppressOn},
		{"1", wakeContentSuppressOn},
		{"true", wakeContentSuppressOn},
		{"yes", wakeContentSuppressOn},
		{"garbage", wakeContentSuppressOff},
	}
	for _, c := range cases {
		t.Setenv(wakeContentSuppressEnv, c.env)
		got := wakeContentSuppressSetting()
		if got != c.want {
			t.Errorf("env=%q: got %s, want %s", c.env, got, c.want)
		}
	}
}

func TestFormatFigureList(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		got := formatFigureList([]string{"0.552", "3660"}, 5)
		want := "0.552, 3660"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("over limit", func(t *testing.T) {
		got := formatFigureList([]string{"0.552", "3660", "2391.2", "3108.56", "1268.8", "575"}, 5)
		want := "0.552, 3660, 2391.2, 3108.56, 1268.8, …(1 more)"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
