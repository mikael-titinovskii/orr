package app

import (
	"fmt"
	"math"
	"strconv"
)

// predictionActiveHoursPerDay is the working day the horizons assume. The hourly
// rate covers only those hours, so the projection describes a coding session
// rather than a proxy left running around the clock.
const predictionActiveHoursPerDay = 8

// predictionWorkdaysPerMonth is the month the label quotes and the 20-day
// horizon projects: 20 working days, so the two figures agree.
const predictionWorkdaysPerMonth = 20

// predictionWorkloadSteps are the token days the 'p' key cycles through, from a
// light session to a heavy one in steps of ten million. The volume is a view
// setting rather than a routing decision, so the choice is not persisted.
var predictionWorkloadSteps = [...]int64{
	10_000_000, 20_000_000, 30_000_000, 40_000_000, 50_000_000,
	60_000_000, 70_000_000, 80_000_000, 90_000_000, 100_000_000,
	110_000_000, 120_000_000, 130_000_000, 140_000_000, 150_000_000,
	160_000_000, 170_000_000, 180_000_000, 190_000_000, 200_000_000,
}

// defaultPredictionWorkloadStep is the step a fresh dashboard starts on.
const defaultPredictionWorkloadStep = 1

// predictionHorizon is one forward window the routing header reports.
type predictionHorizon struct {
	label string
	hours float64
}

var predictionHorizons = [...]predictionHorizon{
	{label: "1h", hours: 1},
	{label: "8h", hours: 8},
	{label: "20d", hours: predictionWorkdaysPerMonth * predictionActiveHoursPerDay},
}

// predictionRate is the token volume one active hour bills, split by billing
// class, plus the requests those tokens arrive in.
type predictionRate struct {
	prompt     float64
	cached     float64
	cacheWrite float64
	completion float64
	requests   float64
}

// newPredictionRate spreads a day of traffic over the assumed working hours and
// the reference request's token mix: the large, mostly cached prompt a coding
// client sends. Both inputs are fixed, so the same provider always projects the
// same cost and only its own published prices can move the figure.
func newPredictionRate(daily float64) predictionRate {
	reference := benchmarkReferenceProfile
	total := reference.promptTokens + reference.cachedTokens + reference.completionTokens
	perHour := daily / predictionActiveHoursPerDay
	scale := perHour / total
	return predictionRate{
		prompt:     reference.promptTokens * scale,
		cached:     reference.cachedTokens * scale,
		cacheWrite: reference.cacheWriteTokens * scale,
		completion: reference.completionTokens * scale,
		requests:   scale,
	}
}

// formatPredictionWorkload labels the line with the day it prices and the month
// that day adds up to over the working month, so the volume reads as a daily
// figure and a monthly one rather than as an abstraction.
func formatPredictionWorkload(tokens int64) string {
	return formatPredictionVolume(tokens) + "/d " +
		formatPredictionVolume(tokens*predictionWorkdaysPerMonth) + "/m"
}

// formatPredictionVolume renders a token count with the largest suffix that
// keeps it short: billions once the figure reaches a thousand million, millions
// below that, so a monthly total never reads as a bare four-digit number.
func formatPredictionVolume(tokens int64) string {
	if tokens >= 1_000_000_000 {
		return strconv.FormatFloat(float64(tokens)/1_000_000_000, 'f', -1, 64) + "b"
	}
	return strconv.FormatFloat(float64(tokens)/1_000_000, 'f', -1, 64) + "m"
}

// predictionStep is the 'p' cycle position, clamped to a configured step so a
// dashboard built without one still prices something valid.
func (m dashboardModel) predictionStepValue() int64 {
	if m.predictionStep < 0 || m.predictionStep >= len(predictionWorkloadSteps) {
		return predictionWorkloadSteps[defaultPredictionWorkloadStep]
	}
	return predictionWorkloadSteps[m.predictionStep]
}

// hourlyCost is what one active hour of the rate costs at pricing. A provider
// with no cache-read price bills the cached tokens at its full input price,
// which is what the workload would really pay there. ok is false when a price
// the workload needs is missing, so an incomplete catalog entry is never
// projected as free.
func (r predictionRate) hourlyCost(pricing endpointPricing) (float64, bool) {
	prompt, cached, cacheWrite := r.prompt, r.cached, r.cacheWrite
	if pricing.InputCacheRead.float() <= 0 {
		prompt += cached + cacheWrite
		cached, cacheWrite = 0, 0
	}
	total := max(pricing.Request.float(), 0) * r.requests
	for _, component := range []struct {
		tokens float64
		price  float64
	}{
		{prompt, pricing.Prompt.float()},
		{cached, pricing.InputCacheRead.float()},
		{cacheWrite, pricing.cacheWrite().float()},
		{r.completion, pricing.Completion.float()},
	} {
		if component.tokens <= 0 {
			continue
		}
		if component.price <= 0 || math.IsNaN(component.price) || math.IsInf(component.price, 0) {
			return 0, false
		}
		total += component.tokens * component.price
	}
	return total, total > 0
}

// predictionSpend formats one hour's cost across every horizon, in the order
// the line lists them: the window, then what it would cost.
func predictionSpend(hourly float64) []string {
	values := make([]string, 0, len(predictionHorizons))
	for _, horizon := range predictionHorizons {
		values = append(values, horizon.label+" "+formatPredictionCost(hourly*horizon.hours))
	}
	return values
}

// formatPredictionCost renders a projected spend compactly: more decimals only
// where cents would hide the figure, and whole dollars once cents stop carrying
// information. Sub-cent values follow the log's own four-decimal cost format.
func formatPredictionCost(cost float64) string {
	switch {
	case cost <= 0:
		return ""
	case cost < 0.01:
		return fmt.Sprintf("$%.4f", cost)
	case cost < 0.1:
		return fmt.Sprintf("$%.3f", cost)
	case cost < 100:
		return fmt.Sprintf("$%.2f", cost)
	}
	return fmt.Sprintf("$%.0f", cost)
}
