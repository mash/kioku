package runner

import "math"

// UsageSample contains provider-reported values only. A nil value is unknown,
// never an estimate or a known zero. It carries no billing/principal metadata.
type UsageSample struct {
	Model                    *string
	InputTokens              *int64
	OutputTokens             *int64
	CacheReadInputTokens     *int64
	CacheCreationInputTokens *int64
	CostCents                *int64
}

func SumUsage(samples []UsageSample) UsageSample {
	if len(samples) == 0 {
		return UsageSample{}
	}
	out := UsageSample{}
	model := samples[0].Model
	if model != nil && *model != "" {
		same := true
		for _, s := range samples {
			if s.Model == nil || *s.Model != *model {
				same = false
				break
			}
		}
		if same {
			v := *model
			out.Model = &v
		}
	}
	fields := []struct {
		get func(UsageSample) *int64
		dst **int64
	}{
		{func(s UsageSample) *int64 { return s.InputTokens }, &out.InputTokens},
		{func(s UsageSample) *int64 { return s.OutputTokens }, &out.OutputTokens},
		{func(s UsageSample) *int64 { return s.CacheReadInputTokens }, &out.CacheReadInputTokens},
		{func(s UsageSample) *int64 { return s.CacheCreationInputTokens }, &out.CacheCreationInputTokens},
		{func(s UsageSample) *int64 { return s.CostCents }, &out.CostCents},
	}
	for _, field := range fields {
		var total int64
		known := true
		for _, s := range samples {
			v := field.get(s)
			if v == nil || *v < 0 || total > math.MaxInt64-*v {
				known = false
				break
			}
			total += *v
		}
		if known {
			*field.dst = &total
		}
	}
	return out
}
