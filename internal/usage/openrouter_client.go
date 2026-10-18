package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

const openRouterBodyLimit = 8 << 20

var errOpenRouterData = errors.New("invalid or incomplete OpenRouter response")

type openRouterClient struct {
	client *http.Client
	base   string
}
type openRouterObject map[string]json.RawMessage

func (c openRouterClient) request(ctx context.Context, key, path string, payload any) (openRouterObject, error) {
	method := http.MethodGet
	var body []byte
	if payload != nil {
		method = http.MethodPost
		body, _ = json.Marshal(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("could not create OpenRouter request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("OpenRouter request failed (network or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenRouter API returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openRouterBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("could not read OpenRouter response")
	}
	if len(data) > openRouterBodyLimit {
		return nil, fmt.Errorf("OpenRouter response exceeded size limit")
	}
	var envelope openRouterObject
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errOpenRouterData
	}
	return openRouterNested(envelope, "data")
}
func openRouterNested(o openRouterObject, name string) (openRouterObject, error) {
	var result openRouterObject
	if json.Unmarshal(o[name], &result) != nil || result == nil {
		return nil, errOpenRouterData
	}
	return result, nil
}
func openRouterCount(raw json.RawMessage) (int64, error) {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return 0, errOpenRouterData
		}
	}
	// Validate JSON numeric grammar before exact arithmetic (no float rounding).
	if len(text) > 128 || !json.Valid([]byte(text)) || text == "null" {
		return 0, errOpenRouterData
	}
	// Bound exponents before big.Rat allocation, even for malicious numeric strings.
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent < -128 || exponent > 128 {
			return 0, errOpenRouterData
		}
	}
	number, ok := new(big.Rat).SetString(text)
	if !ok || !number.IsInt() || number.Sign() < 0 || !number.Num().IsInt64() {
		return 0, errOpenRouterData
	}
	return number.Num().Int64(), nil
}
func openRouterMoney(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || string(raw) == "null" || raw[0] == '"' {
		return 0, errOpenRouterData
	}
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errOpenRouterData
	}
	return value, nil
}
func openRouterMoneyFields(o openRouterObject, fields map[string]*float64) error {
	for name, target := range fields {
		value, err := openRouterMoney(o[name])
		if err != nil {
			return err
		}
		*target = value
	}
	return nil
}
func (c openRouterClient) credits(ctx context.Context, key string) (*OpenRouterBalance, error) {
	o, err := c.request(ctx, key, "/credits", nil)
	if err != nil {
		return nil, err
	}
	b := &OpenRouterBalance{}
	if err = openRouterMoneyFields(o, map[string]*float64{"total_credits": &b.TotalCredits, "total_usage": &b.TotalUsage}); err != nil {
		return nil, err
	}
	b.Remaining = b.TotalCredits - b.TotalUsage
	return b, nil
}
func (c openRouterClient) key(ctx context.Context, key string) (*OpenRouterKeyUsage, error) {
	o, err := c.request(ctx, key, "/key", nil)
	if err != nil {
		return nil, err
	}
	var management *bool
	if json.Unmarshal(o["is_management_key"], &management) != nil || management == nil {
		return nil, errOpenRouterData
	}
	if *management {
		return nil, nil
	}
	k := &OpenRouterKeyUsage{Scope: "key"}
	err = openRouterMoneyFields(o, map[string]*float64{"usage": &k.Usage, "usage_daily": &k.UsageDaily, "usage_weekly": &k.UsageWeekly, "usage_monthly": &k.UsageMonthly})
	if err != nil {
		return nil, err
	}
	optional := map[string]**float64{"byok_usage": &k.BYOKUsage, "byok_usage_daily": &k.BYOKUsageDaily, "byok_usage_weekly": &k.BYOKUsageWeekly, "byok_usage_monthly": &k.BYOKUsageMonthly, "limit": &k.Limit, "limit_remaining": &k.LimitRemaining}
	for name, target := range optional {
		raw := o[name]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		value, err := openRouterMoney(raw)
		if err != nil {
			return nil, err
		}
		*target = &value
	}
	if raw := o["limit_reset"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &k.LimitReset) != nil {
			return nil, errOpenRouterData
		}
	}
	return k, nil
}
