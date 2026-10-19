package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cronField struct {
	any    bool
	values map[int]bool
}

type cronSpec struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

// ValidateCronExpression reports whether expr is a supported 5-field cron expression.
func ValidateCronExpression(expr string) error {
	_, err := parseCronExpression(expr)
	return err
}

func parseCronExpression(expr string) (cronSpec, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return cronSpec{}, fmt.Errorf("cron expression must have 5 fields")
	}
	minute, err := parseCronField(parts[0], 0, 59)
	if err != nil {
		return cronSpec{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseCronField(parts[1], 0, 23)
	if err != nil {
		return cronSpec{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseCronField(parts[2], 1, 31)
	if err != nil {
		return cronSpec{}, fmt.Errorf("day-of-month: %w", err)
	}
	month, err := parseCronField(parts[3], 1, 12)
	if err != nil {
		return cronSpec{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseCronField(parts[4], 0, 6)
	if err != nil {
		return cronSpec{}, fmt.Errorf("day-of-week: %w", err)
	}
	return cronSpec{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

func parseCronField(raw string, min, max int) (cronField, error) {
	raw = strings.TrimSpace(raw)
	if raw == "*" {
		return cronField{any: true}, nil
	}
	values := make(map[int]bool)
	segments := strings.Split(raw, ",")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return cronField{}, fmt.Errorf("empty segment")
		}
		if strings.HasPrefix(seg, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(seg, "*/"))
			if err != nil || step <= 0 {
				return cronField{}, fmt.Errorf("invalid step %q", seg)
			}
			for i := min; i <= max; i += step {
				values[i] = true
			}
			continue
		}
		if strings.Contains(seg, "-") {
			r := strings.SplitN(seg, "-", 2)
			if len(r) != 2 {
				return cronField{}, fmt.Errorf("invalid range %q", seg)
			}
			start, err1 := strconv.Atoi(strings.TrimSpace(r[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(r[1]))
			if err1 != nil || err2 != nil || start > end {
				return cronField{}, fmt.Errorf("invalid range %q", seg)
			}
			if start < min || end > max {
				return cronField{}, fmt.Errorf("range %q out of bounds", seg)
			}
			for i := start; i <= end; i++ {
				values[i] = true
			}
			continue
		}
		v, err := strconv.Atoi(seg)
		if err != nil {
			return cronField{}, fmt.Errorf("invalid value %q", seg)
		}
		if v < min || v > max {
			return cronField{}, fmt.Errorf("value %d out of bounds", v)
		}
		values[v] = true
	}
	return cronField{values: values}, nil
}

func (f cronField) match(v int) bool {
	if f.any {
		return true
	}
	return f.values[v]
}

// NextCronTime returns the first fire time strictly after the given instant.
func NextCronTime(expr, tz string, after time.Time) (time.Time, error) {
	spec, err := parseCronExpression(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	max := t.Add(366 * 24 * time.Hour)
	for !t.After(max) {
		dow := int(t.Weekday())
		if spec.minute.match(t.Minute()) && spec.hour.match(t.Hour()) && spec.month.match(int(t.Month())) && spec.dom.match(t.Day()) && spec.dow.match(dow) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("no cron fire time found within one year")
}

// EffectiveCronTimezone prefers the job schedule timezone over the trigger timezone.
func EffectiveCronTimezone(cfgTZ, scheduleTZ string) string {
	if tz := NormalizeTZ(scheduleTZ); tz != "" {
		return tz
	}
	return NormalizeTZ(cfgTZ)
}

// NormalizeTZ trims whitespace from a timezone name.
func NormalizeTZ(v string) string {
	return strings.TrimSpace(v)
}
