package assertion

import (
	"sort"
	"sync"
	"time"

	"go.ddosify.com/ddosify/core/scenario/scripting/assertion"
	"go.ddosify.com/ddosify/core/scenario/scripting/assertion/evaluator"
	"go.ddosify.com/ddosify/core/types"
	"golang.org/x/exp/slices"
)

var tickerInterval = 100 // interval in millisecond
type AssertionService struct {
	assertions map[string]types.TestAssertionOpt // Rule -> Opts
	resultChan chan *types.ScenarioResult
	abortChan  chan struct{}
	doneChan   chan TestAssertionResult
	assertEnv  *evaluator.AssertEnv
	abortTick  map[string]int // rule -> tickIndex
	iterCount  int
	mu         sync.Mutex
}

type TestAssertionResult struct {
	Fail        bool
	FailedRules []FailedRule
}

type FailedRule struct {
	Rule        string                 `json:"rule"`
	ReceivedMap map[string]interface{} `json:"received"`
}

func NewAssertionService() (service *AssertionService) {
	return &AssertionService{}
}

func (as *AssertionService) Init(assertions map[string]types.TestAssertionOpt) chan struct{} {
	as.assertions = assertions
	abortChan := make(chan struct{})
	as.abortChan = abortChan
	doneChan := make(chan TestAssertionResult)
	as.doneChan = doneChan
	totalTime := make([]int64, 0)
	as.assertEnv = &evaluator.AssertEnv{TotalTime: totalTime}
	as.abortTick = make(map[string]int)
	as.mu = sync.Mutex{}
	return as.abortChan
}

func (as *AssertionService) GetTotalTimes() []int64 {
	return as.assertEnv.TotalTime
}
func (as *AssertionService) GetFailCount() int {
	return as.assertEnv.FailCount
}

func (as *AssertionService) Start(input chan *types.ScenarioResult) {
	// get iteration results, add store them cumulatively
	firstResult := true
	for r := range input {
		as.mu.Lock()
		as.aggregate(r)
		as.mu.Unlock()

		// after first result start checking assertions
		if firstResult {
			go as.applyAssertions()
			firstResult = false
		}
	}
	as.doneChan <- as.giveFinalResult()
}

func (as *AssertionService) aggregate(r *types.ScenarioResult) {
	var iterationTime int64
	var iterFailed bool
	as.iterCount++
	for _, sr := range r.StepResults {
		iterationTime += sr.Duration.Milliseconds()
		if sr.Err.Type != "" || len(sr.FailedAssertions) > 0 {
			iterFailed = true
		}
	}
	if iterFailed {
		as.assertEnv.FailCount++
	}

	// keep totalTime array sorted
	as.insertSorted(iterationTime)

	as.assertEnv.FailCountPerc = float64(as.assertEnv.FailCount) / float64(as.iterCount)
}

func (as *AssertionService) applyAssertions() {
	ticker := time.NewTicker(time.Duration(tickerInterval) * time.Millisecond)
	tickIndex := 1
	// apply assertions on the fly for only abort:true ones
	assertionsWithAbort := make(map[string]types.TestAssertionOpt)
	for rule, opts := range as.assertions {
		if opts.Abort {
			assertionsWithAbort[rule] = opts
		}
	}
	for range ticker.C {
		as.mu.Lock()
		var totalTime []int64
		totalTime = append(totalTime, as.assertEnv.TotalTime...)
		assertEnv := evaluator.AssertEnv{
			TotalTime: totalTime,
			FailCount: as.assertEnv.FailCount,
		}
		as.mu.Unlock()

		// apply assertions
		for rule, opts := range assertionsWithAbort {
			res, _ := assertion.Assert(rule, &assertEnv)
			if res == false && opts.Abort {
				// if delay is zero, immediately abort
				if opts.Delay == 0 || as.abortTick[rule] == tickIndex {
					as.abortChan <- struct{}{}
					return
				}
				if _, ok := as.abortTick[rule]; !ok {
					// schedule check at
					delayTick := (time.Duration(opts.Delay) * time.Second) / (time.Duration(tickerInterval) * time.Millisecond)
					as.abortTick[rule] = tickIndex + int(delayTick) - 1
				}
			}
		}
		tickIndex++
	}
}

func (as *AssertionService) giveFinalResult() TestAssertionResult {
	// return final result
	result := TestAssertionResult{
		Fail: false,
	}
	failedRules := []FailedRule{}
	for rule, _ := range as.assertions {
		res, err := assertion.Assert(rule, as.assertEnv)
		if res == false {
			failedRules = append(failedRules, FailedRule{
				Rule:        rule,
				ReceivedMap: err.(assertion.AssertionError).Received(),
			})
		}
	}

	if len(failedRules) > 0 {
		result.Fail = true
		result.FailedRules = failedRules
	}

	return result
}

func (as *AssertionService) Done() chan TestAssertionResult {
	return as.doneChan
}

func (as *AssertionService) insertSorted(v int64) {
	index := sort.Search(len(as.assertEnv.TotalTime), func(i int) bool { return as.assertEnv.TotalTime[i] >= v })
	as.assertEnv.TotalTime = slices.Insert(as.assertEnv.TotalTime, index, v)
}
