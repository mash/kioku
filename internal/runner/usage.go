package runner

import "github.com/mash/kioku/internal/model"

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
	modelID := samples[0].Model
	if modelID != nil && *modelID != "" {
		same := true
		for _, s := range samples {
			if s.Model == nil || *s.Model != *modelID {
				same = false
				break
			}
		}
		if same {
			v := *modelID
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
	values := make([]*int64, len(samples))
	for _, field := range fields {
		for i, sample := range samples {
			values[i] = field.get(sample)
		}
		*field.dst = model.SumMeasurements(values...)
	}
	return out
}
