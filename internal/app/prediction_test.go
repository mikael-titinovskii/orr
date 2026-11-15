package app

import (
	"math"
	"reflect"
	"testing"
)

func TestPredictionWorkloadIsTheReferenceDay(t *testing.T) {
	// The default day of tokens over eight active hours, split the reference
	// request's 2,000 / 30,000 / 200 way.
	daily := float64(predictionWorkloadSteps[defaultPredictionWorkloadStep])
	rate := newPredictionRate(daily)
	perHour := daily / predictionActiveHoursPerDay
	const total = 32_200.0
	for _, component := range []struct {
		name string
		got  float64
		want float64
	}{
		{"prompt", rate.prompt, perHour * 2_000 / total},
		{"cached", rate.cached, perHour * 30_000 / total},
		{"completion", rate.completion, perHour * 200 / total},
		{"requests", rate.requests, perHour / total},
	} {
		if !closeTo(component.got, component.want) {
			t.Errorf("%s per hour = %v, want %v", component.name, component.got, component.want)
		}
	}
}

func TestFormatPredictionWorkload(t *testing.T) {
	for tokens, want := range map[int64]string{
		10_000_000:  "10m/d 200m/m",
		20_000_000:  "20m/d 400m/m",
		30_000_000:  "30m/d 600m/m",
		80_000_000:  "80m/d 1.6b/m",
		200_000_000: "200m/d 4b/m",
		1_500_000:   "1.5m/d 30m/m",
	} {
		if got := formatPredictionWorkload(tokens); got != want {
			t.Errorf("formatPredictionWorkload(%d) = %q, want %q", tokens, got, want)
		}
	}
}

func TestPredictionWorkloadStepsAdvanceByTheChosenDays(t *testing.T) {
	want := []int64{50, 100, 300, 500, 700, 1000}
	if len(predictionWorkloadSteps) != len(want) {
		t.Fatalf("workload steps = %v, want %v million", predictionWorkloadSteps, want)
	}
	for i, millions := range want {
		if got := predictionWorkloadSteps[i]; got != millions*1_000_000 {
			t.Fatalf("step %d = %d tokens, want %dm", i, got, millions)
		}
	}
	if got := predictionWorkloadSteps[defaultPredictionWorkloadStep]; got != 100_000_000 {
		t.Fatalf("default workload = %d, want 100m", got)
	}
}

func TestPredictionStepForTokensSnapsToNearestStop(t *testing.T) {
	for tokens, want := range map[int64]int{
		50_000_000:    0,
		100_000_000:   1,
		1_000_000_000: 5,
		420_000_000:   3, // nearer 500m than 300m
		650_000_000:   4, // nearer 700m than 500m
	} {
		if got := predictionStepForTokens(tokens); got != want {
			t.Errorf("predictionStepForTokens(%d) = %d, want %d", tokens, got, want)
		}
	}
}

func TestPredictionRateHourlyCostPricesEveryTokenClass(t *testing.T) {
	rate := predictionRate{prompt: 1_000_000, cached: 1_000_000, completion: 1_000_000}
	got, ok := rate.hourlyCost(endpointPricing{Prompt: 0.000001, Completion: 0.000004, InputCacheRead: 0.0000002})
	if !ok {
		t.Fatal("no cost for complete pricing")
	}
	// $1/M input + $0.20/M cache read + $4/M output.
	if want := 5.2; !closeTo(got, want) {
		t.Fatalf("hourly cost = $%v, want $%v", got, want)
	}
}

func TestPredictionRateHourlyCostBillsCacheReadsAtTheInputPriceWhenUnpriced(t *testing.T) {
	rate := predictionRate{prompt: 1_000_000, cached: 1_000_000, completion: 1_000_000}
	got, ok := rate.hourlyCost(endpointPricing{Prompt: 0.000001, Completion: 0.000004})
	if !ok {
		t.Fatal("no cost without a cache-read price")
	}
	// 2M input tokens at $1/M plus 1M output at $4/M.
	if want := 6.0; !closeTo(got, want) {
		t.Fatalf("hourly cost = $%v, want $%v", got, want)
	}
}

func TestPredictionRateHourlyCostRejectsIncompletePricing(t *testing.T) {
	rate := predictionRate{prompt: 1000, completion: 1000}
	if _, ok := rate.hourlyCost(endpointPricing{Prompt: 0.000001}); ok {
		t.Fatal("projected a cost without an output price")
	}
}

// The $Mon column and the projection's 20d horizon price the same working
// month, so the two must agree.
func TestPredictionMonthlyCostIsTheTwentyDayHorizon(t *testing.T) {
	rate := newPredictionRate(100_000_000)
	pricing := endpointPricing{Prompt: 0.000001, Completion: 0.000004, InputCacheRead: 0.0000002}
	hourly, ok := rate.hourlyCost(pricing)
	if !ok {
		t.Fatal("no hourly cost for complete pricing")
	}
	monthly, ok := rate.monthlyCost(pricing)
	if !ok {
		t.Fatal("no monthly cost for complete pricing")
	}
	if want := hourly * predictionMonthHours; !closeTo(monthly, want) {
		t.Fatalf("monthly cost = $%v, want $%v", monthly, want)
	}
	if _, ok := rate.monthlyCost(endpointPricing{Prompt: 0.000001}); ok {
		t.Fatal("projected a monthly cost without an output price")
	}
}

func TestPredictionSpendCoversEveryHorizon(t *testing.T) {
	got := predictionSpend(1.3)
	want := []string{"1h $1.30", "8h $10.40", "20d $208"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prediction spend = %q, want %q", got, want)
	}
}

func TestFormatPredictionCostKeepsSmallFiguresReadable(t *testing.T) {
	for cost, want := range map[float64]string{
		0:       "",
		0.0024:  "$0.0024",
		0.024:   "$0.024",
		0.24:    "$0.24",
		4.86:    "$4.86",
		208.412: "$208",
	} {
		if got := formatPredictionCost(cost); got != want {
			t.Errorf("formatPredictionCost(%v) = %q, want %q", cost, got, want)
		}
	}
}

func closeTo(got, want float64) bool {
	return math.Abs(got-want) <= math.Abs(want)*1e-9
}
