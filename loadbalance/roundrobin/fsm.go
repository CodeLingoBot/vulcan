package roundrobin

import (
	"fmt"
	timetools "github.com/mailgun/gotools-time"
	"math"
	"sort"
	"time"
)

// This handler increases weights on endpoints that perform better than others
// it also rolls back to original weights if the endpoints have changed.
type FSMHandler struct {
	// As usual, control time in tests
	timeProvider timetools.TimeProvider
	// Time that freezes state machine to accumulate stats after updating the weights
	backoffDuration time.Duration
	// Timer is set to give probing some time to take place
	timer time.Time
	// Endpoints for this round
	endpoints []*WeightedEndpoint
	// Precalculated original weights
	originalWeights []SuggestedWeight
	// Last returned weights
	lastWeights []SuggestedWeight
}

const (
	// This is the maximum weight that handler will set for the endpoint
	FSMMaxWeight = 4096
	// Multiplier for the endpoint weight
	FSMGrowFactor = 16
)

func NewFSMHandler() (*FSMHandler, error) {
	return NewFSMHandlerWithOptions(&timetools.RealTime{})
}

func NewFSMHandlerWithOptions(timeProvider timetools.TimeProvider) (*FSMHandler, error) {
	if timeProvider == nil {
		return nil, fmt.Errorf("time provider can not be nil")
	}
	return &FSMHandler{
		timeProvider: timeProvider,
	}, nil
}

func (fsm *FSMHandler) Init(endpoints []*WeightedEndpoint) {
	fsm.originalWeights = makeOriginalWeights(endpoints)
	fsm.lastWeights = fsm.originalWeights
	fsm.endpoints = endpoints
	if len(endpoints) > 0 {
		fsm.backoffDuration = endpoints[0].meter.GetWindowSize() / 2
	}
	fsm.timer = fsm.timeProvider.UtcNow().Add(-1 * time.Second)
}

// Called on every load balancer NextEndpoint call, returns the suggested weights
// on every call, can adjust weights if needed.
func (fsm *FSMHandler) AdjustWeights() ([]SuggestedWeight, error) {
	// In this case adjusting weights would have no effect, so do nothing
	if len(fsm.endpoints) < 2 {
		return fsm.originalWeights, nil
	}
	// Metrics are not ready
	if !metricsReady(fsm.endpoints) {
		return fsm.originalWeights, nil
	}
	if !fsm.timerExpired() {
		return fsm.lastWeights, nil
	}
	// Select endpoints with highest error rates and lower their weight
	good, bad := splitEndpoints(fsm.endpoints)
	// No endpoints that are different by their quality, so converge weights
	if len(bad) == 0 || len(good) == 0 {
		weights, changed := fsm.convergeWeights()
		if changed {
			fsm.lastWeights = weights
			fsm.setTimer()
		}
		return fsm.lastWeights, nil
	}
	fsm.lastWeights = fsm.adjustWeights(good, bad)
	fsm.setTimer()
	return fsm.lastWeights, nil
}

func (fsm *FSMHandler) convergeWeights() ([]SuggestedWeight, bool) {
	weights := make([]SuggestedWeight, len(fsm.endpoints))
	// If we have previoulsy changed endpoints try to restore weights to the original state
	changed := false
	for i, e := range fsm.endpoints {
		weights[i] = &EndpointWeight{e, decrease(e.GetOriginalWeight(), e.GetEffectiveWeight())}
		if e.GetEffectiveWeight() != e.GetOriginalWeight() {
			changed = true
		}
	}
	return normalizeWeights(weights), changed
}

func (fsm *FSMHandler) adjustWeights(good map[string]bool, bad map[string]bool) []SuggestedWeight {
	// Increase weight on good endpoints
	weights := make([]SuggestedWeight, len(fsm.endpoints))
	for i, e := range fsm.endpoints {
		if good[e.GetId()] && increase(e.GetEffectiveWeight()) <= FSMMaxWeight {
			weights[i] = &EndpointWeight{e, increase(e.GetEffectiveWeight())}
		} else {
			weights[i] = &EndpointWeight{e, e.GetEffectiveWeight()}
		}
	}
	return normalizeWeights(weights)
}

func weightsGcd(weights []SuggestedWeight) int {
	divisor := -1
	for _, w := range weights {
		if divisor == -1 {
			divisor = w.GetWeight()
		} else {
			divisor = gcd(divisor, w.GetWeight())
		}
	}
	return divisor
}

func normalizeWeights(weights []SuggestedWeight) []SuggestedWeight {
	gcd := weightsGcd(weights)
	if gcd <= 1 {
		return weights
	}
	for _, w := range weights {
		w.SetWeight(w.GetWeight() / gcd)
	}
	return weights
}

func (fsm *FSMHandler) setTimer() {
	fsm.timer = fsm.timeProvider.UtcNow().Add(fsm.backoffDuration)
}

func (fsm *FSMHandler) timerExpired() bool {
	return fsm.timer.Before(fsm.timeProvider.UtcNow())
}

func metricsReady(endpoints []*WeightedEndpoint) bool {
	for _, e := range endpoints {
		if !e.meter.IsReady() {
			return false
		}
	}
	return true
}

func increase(weight int) int {
	return weight * FSMGrowFactor
}

func decrease(target, current int) int {
	adjusted := current / FSMGrowFactor
	if adjusted < target {
		return target
	} else {
		return adjusted
	}
}

func makeOriginalWeights(endpoints []*WeightedEndpoint) []SuggestedWeight {
	weights := make([]SuggestedWeight, len(endpoints))
	for i, e := range endpoints {
		weights[i] = &EndpointWeight{
			Weight:   e.GetOriginalWeight(),
			Endpoint: e,
		}
	}
	return weights
}

// Splits endpoint into two groups of endpoints with bad performance and good performance. It does compare relative
// performances of the endpoints though, so if all endpoints have the same performance,
func splitEndpoints(endpoints []*WeightedEndpoint) (map[string]bool, map[string]bool) {
	good, bad := make(map[string]bool), make(map[string]bool)

	// In case of event amount of endpoints, the algo below won't be able to do anything smart.
	// to overcome this, we add a third endpoint that is same to the "best" endpoint of those two given to resolve potential ambiguity
	var newEndpoints []*WeightedEndpoint
	if len(endpoints)%2 == 0 {
		newEndpoints = make([]*WeightedEndpoint, len(endpoints)+1)
		copy(newEndpoints, endpoints)
		newEndpoints[len(endpoints)] = min(endpoints)
	} else {
		newEndpoints = endpoints
	}

	m := medianEndpoint(newEndpoints)
	mAbs := medianAbsoluteDeviation(newEndpoints)
	for _, e := range endpoints {
		if e.failRate() > m+mAbs*1.5 {
			bad[e.GetId()] = true
		} else {
			good[e.GetId()] = true
		}
	}
	return good, bad
}

func medianEndpoint(values []*WeightedEndpoint) float64 {
	vals := make([]*WeightedEndpoint, len(values))
	copy(vals, values)
	sort.Sort(WeightedEndpoints(vals))
	l := len(vals)
	if l%2 != 0 {
		return vals[l/2].failRate()
	} else {
		return (vals[l/2-1].failRate() + vals[l/2].failRate()) / 2.0
	}
}

func median(values []float64) float64 {
	sort.Float64s(values)
	l := len(values)
	if l%2 != 0 {
		return values[l/2]
	} else {
		return (values[l/2-1] + values[l/2]) / 2.0
	}
}

func medianAbsoluteDeviation(values []*WeightedEndpoint) float64 {
	m := medianEndpoint(values)
	distances := make([]float64, len(values))
	for i, v := range values {
		distances[i] = math.Abs(v.failRate() - m)
	}
	return median(distances)
}

func min(values []*WeightedEndpoint) *WeightedEndpoint {
	val := values[0]
	for _, v := range values {
		if v.failRate() < val.failRate() {
			val = v
		}
	}
	return val
}
