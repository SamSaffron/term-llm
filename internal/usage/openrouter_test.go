package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openRouterTestOptions() OpenRouterOptions {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	return OpenRouterOptions{ManagementKey: "management-secret", Since: now.AddDate(0, 0, -2), Until: now, Now: now}
}
func openRouterTestRow(model, date string, cost float64) map[string]any {
	return map[string]any{"model": model, "created_at__day": date, "request_count": "2", "tokens_prompt": 100, "tokens_completion": "20", "cached_tokens": "90", "reasoning_tokens": 10, "total_usage": cost, "byok_usage": 0, "credits_usage": cost, "byok_fees": 0}
}
func openRouterTestEnvelope(rows []map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"data": rows, "metadata": map[string]any{"row_count": len(rows), "truncated": false}, "cachedAt": 1792282964859}}
}

const openRouterTestKey = `{"data":{"is_management_key":false,"usage":12,"usage_daily":1,"usage_weekly":5,"usage_monthly":10,"limit":null,"limit_remaining":null,"limit_reset":null}}`
const openRouterTestCredits = `{"data":{"total_credits":30,"total_usage":12}}`

func TestOpenRouterAnalytics(t *testing.T) {
	rows := []map[string]any{openRouterTestRow("z", "2026-10-18", 1), openRouterTestRow("b", "2026-10-17", 2), openRouterTestRow("a", "2026-10-17", 2)}
	rows[1]["date__day"] = "2026-10-17"
	delete(rows[1], "created_at__day")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-secret" {
			t.Error("wrong credential")
		}
		switch r.URL.Path {
		case "/analytics/query":
			if r.Method != "POST" {
				t.Error("wrong method")
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["granularity"] != "day" || payload["limit"] != float64(1000) {
				t.Error("invalid query")
			}
			json.NewEncoder(w).Encode(openRouterTestEnvelope(rows))
		case "/credits":
			fmt.Fprint(w, openRouterTestCredits)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	o := openRouterTestOptions()
	o.APIKey = func() (string, error) { return "", errors.New("secret-error") }
	report, err := fetchOpenRouterUsage(context.Background(), o, s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "complete" || report.Totals.Requests != 6 || report.Totals.InputTokens != 300 || report.Totals.OutputTokens != 60 || report.Totals.CostUSD != 5 {
		t.Fatalf("bad report: %+v", report)
	}
	if report.Models[0].Model != "a" || report.Models[1].Model != "b" || report.Daily[0].Date != "2026-10-17" || report.Balance.Remaining != 18 || report.CachedAt == nil {
		t.Fatalf("bad aggregates: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("secret leaked")
	}
}

func TestOpenRouterFallback(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/analytics/query":
					w.WriteHeader(status)
					fmt.Fprint(w, "management-secret body")
				case "/key":
					if r.Header.Get("Authorization") != "Bearer regular-secret" {
						t.Error("wrong key scope")
					}
					fmt.Fprint(w, openRouterTestKey)
				case "/credits":
					fmt.Fprint(w, openRouterTestCredits)
				default:
					t.Error("unexpected endpoint")
				}
			}))
			defer s.Close()
			o := openRouterTestOptions()
			o.APIKey = func() (string, error) { return "regular-secret", nil }
			report, err := fetchOpenRouterUsage(context.Background(), o, s.Client(), s.URL)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != "summary_only" || report.Totals != nil || report.KeyUsage.Scope != "key" || report.KeyUsage.BYOKUsage != nil {
				t.Fatalf("bad fallback: %+v", report)
			}
			encoded, _ := json.Marshal(report)
			for _, part := range []string{`"models":[]`, `"daily":[]`, `"totals":null`, `"byok_usage_usd":null`} {
				if !strings.Contains(string(encoded), part) {
					t.Errorf("missing %s", part)
				}
			}
			if strings.Contains(string(encoded), "secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestOpenRouterEmptyAndOptionalFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/analytics/query" {
			json.NewEncoder(w).Encode(openRouterTestEnvelope([]map[string]any{}))
		} else {
			w.WriteHeader(403)
		}
	}))
	defer s.Close()
	report, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
	if err != nil || report.Status != "complete" || report.Totals == nil || *report.Totals != (OpenRouterAmounts{}) || report.Balance != nil || len(report.Warnings) == 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestOpenRouterStrictCounts(t *testing.T) {
	for _, raw := range []string{`0`, `"0"`, `42`, `"42"`, `2.0`, `2e2`, `9223372036854775807`, `"9007199254740993"`} {
		if _, err := openRouterCount(json.RawMessage(raw)); err != nil {
			t.Errorf("rejected %s", raw)
		}
	}
	for _, raw := range []string{"", `null`, `true`, `-1`, `1.5`, `"NaN"`, `"1/2"`, `9223372036854775808`, `"9223372036854775808"`, `"1e999999999"`, `"0x10"`, `""`} {
		if _, err := openRouterCount(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestOpenRouterIncompleteAnalytics(t *testing.T) {
	tests := map[string]func(map[string]any, map[string]any){
		"missing count":        func(_ map[string]any, r map[string]any) { delete(r, "request_count") },
		"missing money":        func(_ map[string]any, r map[string]any) { delete(r, "credits_usage") },
		"null money":           func(_ map[string]any, r map[string]any) { r["total_usage"] = nil },
		"negative money":       func(_ map[string]any, r map[string]any) { r["total_usage"] = -1 },
		"bad model":            func(_ map[string]any, r map[string]any) { r["model"] = "" },
		"bad date":             func(_ map[string]any, r map[string]any) { r["created_at__day"] = "2026-02-30" },
		"outside range":        func(_ map[string]any, r map[string]any) { r["created_at__day"] = "2026-10-19" },
		"alias conflict":       func(_ map[string]any, r map[string]any) { r["date__day"] = "2026-10-17" },
		"truncated":            func(d map[string]any, _ map[string]any) { d["metadata"].(map[string]any)["truncated"] = true },
		"missing truncated":    func(d map[string]any, _ map[string]any) { delete(d["metadata"].(map[string]any), "truncated") },
		"wrong row count":      func(d map[string]any, _ map[string]any) { d["metadata"].(map[string]any)["row_count"] = 2 },
		"missing rows":         func(d map[string]any, _ map[string]any) { delete(d, "data") },
		"null rows":            func(d map[string]any, _ map[string]any) { d["data"] = nil },
		"missing metadata":     func(d map[string]any, _ map[string]any) { delete(d, "metadata") },
		"bad cached timestamp": func(d map[string]any, _ map[string]any) { d["cachedAt"] = "9223372036854775807" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			row := openRouterTestRow("model", "2026-10-18", 1)
			envelope := openRouterTestEnvelope([]map[string]any{row})
			change(envelope["data"].(map[string]any), row)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/analytics/query" {
					json.NewEncoder(w).Encode(envelope)
				} else {
					fmt.Fprint(w, openRouterTestCredits)
				}
			}))
			defer s.Close()
			report, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
			if err != nil || report.Status != "summary_only" || report.Totals != nil || len(report.Models) != 0 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestOpenRouterTransportSafety(t *testing.T) {
	for name, body := range map[string]string{"bad JSON": "secret {", "missing envelope": "{}", "null envelope": `{"data":null}`, "trailing JSON": `{"data":{}} {}`, "oversized": strings.Repeat("x", openRouterBodyLimit+1)} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			_, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	_, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), redirect.Client(), redirect.URL)
	if err == nil || hits != 0 {
		t.Fatalf("redirect followed: %d %v", hits, err)
	}
}

func TestOpenRouterCancellationAndValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fetchOpenRouterUsage(ctx, openRouterTestOptions(), http.DefaultClient, "http://127.0.0.1:1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
	for _, modify := range []func(*OpenRouterOptions){func(o *OpenRouterOptions) { o.Since = time.Time{} }, func(o *OpenRouterOptions) { o.Until = o.Since }, func(o *OpenRouterOptions) { o.Until = o.Now.Add(time.Second) }} {
		o := openRouterTestOptions()
		modify(&o)
		if _, err := fetchOpenRouterUsage(context.Background(), o, http.DefaultClient, "invalid"); err == nil {
			t.Fatal("invalid range accepted")
		}
	}
}

func TestOpenRouterSummaryOnlyAndManagementSuppression(t *testing.T) {
	for _, management := range []bool{false, true} {
		t.Run(fmt.Sprint(management), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/key":
					fmt.Fprint(w, strings.Replace(openRouterTestKey, `false`, fmt.Sprint(management), 1))
				case "/credits":
					fmt.Fprint(w, openRouterTestCredits)
				default:
					t.Error("analytics called without management key")
				}
			}))
			defer s.Close()
			o := openRouterTestOptions()
			o.ManagementKey = ""
			o.APIKey = func() (string, error) { return "regular-secret", nil }
			report, err := fetchOpenRouterUsage(context.Background(), o, s.Client(), s.URL)
			if err != nil || report.Status != "summary_only" || (report.KeyUsage == nil) != management {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestOpenRouterOverflowAndDeterminism(t *testing.T) {
	if _, err := openRouterAdd(OpenRouterAmounts{Requests: math.MaxInt64}, OpenRouterAmounts{Requests: 1}); err == nil {
		t.Fatal("integer overflow")
	}
	if _, err := openRouterAdd(OpenRouterAmounts{CostUSD: math.MaxFloat64}, OpenRouterAmounts{CostUSD: math.MaxFloat64}); err == nil {
		t.Fatal("float overflow")
	}
	rows := []map[string]any{openRouterTestRow("m", "2026-10-18", 1e16), openRouterTestRow("m", "2026-10-18", 1), openRouterTestRow("m", "2026-10-18", 1)}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/analytics/query" {
			json.NewEncoder(w).Encode(openRouterTestEnvelope(rows))
		} else {
			fmt.Fprint(w, openRouterTestCredits)
		}
	}))
	defer s.Close()
	first, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	rows[0], rows[2] = rows[2], rows[0]
	second, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("row ordering changed aggregate")
	}
}

func TestOpenRouterDateAliases(t *testing.T) {
	for _, timestamp := range []string{"2026-10-18", "2026-10-18T00:00:00.000Z", "2026-10-18T00:00:00+00:00"} {
		row := openRouterObject{"created_at__day": json.RawMessage(`"2026-10-18"`), "date__day": json.RawMessage(strconv.Quote(timestamp))}
		date, err := openRouterDate(row, openRouterTestOptions())
		if err != nil || date != "2026-10-18" {
			t.Fatalf("%s: %s %v", timestamp, date, err)
		}
	}
	for _, timestamp := range []string{"2026-10-18T01:00:00Z", "2026-10-18T00:00:00.001Z", "2026-10-18T00:00:00+01:00", "2026-10-17T00:00:00Z"} {
		row := openRouterObject{"created_at__day": json.RawMessage(`"2026-10-18"`), "date__day": json.RawMessage(strconv.Quote(timestamp))}
		if _, err := openRouterDate(row, openRouterTestOptions()); err == nil {
			t.Fatalf("accepted %s", timestamp)
		}
	}
}

func TestOpenRouterCreditsRetryAndCancellation(t *testing.T) {
	t.Run("retry regular credential", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/analytics/query":
				json.NewEncoder(w).Encode(openRouterTestEnvelope([]map[string]any{}))
			case "/key":
				fmt.Fprint(w, openRouterTestKey)
			case "/credits":
				if r.Header.Get("Authorization") == "Bearer management-secret" {
					w.WriteHeader(403)
				} else {
					fmt.Fprint(w, openRouterTestCredits)
				}
			}
		}))
		defer s.Close()
		o := openRouterTestOptions()
		o.APIKey = func() (string, error) { return "regular-secret", nil }
		report, err := fetchOpenRouterUsage(context.Background(), o, s.Client(), s.URL)
		if err != nil || report.Balance == nil || report.Status != "complete" {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	})
	t.Run("cancel during analytics", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel() }))
		defer s.Close()
		o := openRouterTestOptions()
		o.APIKey = func() (string, error) { t.Error("resolved fallback after cancellation"); return "", nil }
		if _, err := fetchOpenRouterUsage(ctx, o, s.Client(), s.URL); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation swallowed: %v", err)
		}
	})
}

func TestOpenRouterKeyAndCreditRequiredFields(t *testing.T) {
	for name, body := range map[string]string{
		"missing core counter":       `{"data":{"is_management_key":false,"usage":0,"usage_daily":0,"usage_weekly":0}}`,
		"missing key type":           `{"data":{"usage":0,"usage_daily":0,"usage_weekly":0,"usage_monthly":0}}`,
		"malformed optional counter": `{"data":{"is_management_key":false,"usage":0,"usage_daily":0,"usage_weekly":0,"usage_monthly":0,"byok_usage":-1}}`,
		"missing credit usage":       `{"data":{"total_credits":10}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			o := openRouterTestOptions()
			o.ManagementKey = ""
			o.APIKey = func() (string, error) { return "regular-secret", nil }
			if report, err := fetchOpenRouterUsage(context.Background(), o, s.Client(), s.URL); err == nil {
				t.Fatalf("accepted incomplete summary %+v", report)
			}
		})
	}
}

func TestOpenRouterWeightedCacheHitPercent(t *testing.T) {
	rows := []map[string]any{openRouterTestRow("a", "2026-10-17", 1), openRouterTestRow("b", "2026-10-18", 2), openRouterTestRow("empty", "2026-10-18", 0)}
	rows[0]["tokens_prompt"] = 100
	rows[0]["cached_tokens"] = 0
	rows[1]["tokens_prompt"] = 900
	rows[1]["cached_tokens"] = 900
	rows[2]["tokens_prompt"] = 0
	rows[2]["cached_tokens"] = 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/analytics/query" {
			json.NewEncoder(w).Encode(openRouterTestEnvelope(rows))
			return
		}
		fmt.Fprint(w, openRouterTestCredits)
	}))
	defer s.Close()
	r, err := fetchOpenRouterUsage(context.Background(), openRouterTestOptions(), s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.CacheHitPercent == nil || *r.Totals.CacheHitPercent != 90 {
		t.Fatalf("weighted cache: %+v", r.Totals)
	}
	if r.Models[0].CacheHitPercent == nil || *r.Models[0].CacheHitPercent != 100 {
		t.Fatal("model cache ratio")
	}
	if r.Models[2].CacheHitPercent != nil {
		t.Fatal("zero-input model cache ratio must be unknown")
	}
	if *r.Daily[0].CacheHitPercent != 0 || *r.Daily[1].CacheHitPercent != 100 {
		t.Fatal("daily cache ratios")
	}
	encoded, _ := json.Marshal(r)
	if !strings.Contains(string(encoded), `"cache_hit_percent":90`) || !strings.Contains(string(encoded), `"cache_hit_percent":null`) {
		t.Fatalf("cache JSON: %s", encoded)
	}
}
