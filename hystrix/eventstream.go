package hystrix

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	streamEventBufferSize = 10
)

func NewStreamHandler() *StreamHandler {
	return &StreamHandler{}
}

type StreamHandler struct {
	requests map[*http.Request]chan []byte
	mu       sync.RWMutex
	done     chan struct{}
}

func (sh *StreamHandler) Start() {
	sh.requests = make(map[*http.Request]chan []byte)
	sh.done = make(chan struct{})
	go sh.loop()
}

func (sh *StreamHandler) Stop() {
	close(sh.done)
}

var _ http.Handler = (*StreamHandler)(nil)

func (sh *StreamHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	events := sh.register(req)
	defer sh.unregister(req)
	rw.Header().Add("Content-Type", "text/event-stream")
	for event := range events {
		_, err := rw.Write(event)
		if err != nil {
			return
		}
		if f, ok := rw.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (sh *StreamHandler) loop() {
	tick := time.Tick(1 * time.Second)
	for {
		select {
		case <-tick:
			circuitBreakersMutex.RLock()
			for _, cb := range circuitBreakers {
				sh.publishMetrics(cb)
				sh.publishThreadPools(cb.executorPool)
			}
			circuitBreakersMutex.RUnlock()
		case <-sh.done:
			return
		}
	}
}

func (sh *StreamHandler) publishMetrics(cb *CircuitBreaker) error {
	now := time.Now()
	reqCount := cb.metrics.Requests().Sum(now)
	errCount := cb.metrics.Errors.Sum(now)
	errPct := cb.metrics.ErrorPercent(now)

	eventBytes, err := json.Marshal(&streamCmdMetric{
		Type:           "HystrixCommand",
		Name:           cb.Name,
		Group:          cb.Name,
		Time:           currentTime(),
		ReportingHosts: 1,

		RequestCount:       uint32(reqCount),
		ErrorCount:         uint32(errCount),
		ErrorPct:           uint32(errPct),
		CircuitBreakerOpen: cb.isOpen(),

		RollingCountSuccess:            uint32(cb.metrics.Successes.Sum(now)),
		RollingCountFailure:            uint32(cb.metrics.Failures.Sum(now)),
		RollingCountThreadPoolRejected: uint32(cb.metrics.Rejected.Sum(now)),
		RollingCountShortCircuited:     uint32(cb.metrics.ShortCircuits.Sum(now)),
		RollingCountTimeout:            uint32(cb.metrics.Timeouts.Sum(now)),
		RollingCountFallbackSuccess:    uint32(cb.metrics.FallbackSuccesses.Sum(now)),
		RollingCountFallbackFailure:    uint32(cb.metrics.FallbackFailures.Sum(now)),

		LatencyTotal:       cb.metrics.TotalDuration.Timings(),
		LatencyTotalMean:   cb.metrics.TotalDuration.Mean(),
		LatencyExecute:     cb.metrics.RunDuration.Timings(),
		LatencyExecuteMean: cb.metrics.RunDuration.Mean(),

		// TODO: all hard-coded values should become configurable settings, per circuit

		RollingStatsWindow:         10000,
		ExecutionIsolationStrategy: "THREAD",

		CircuitBreakerEnabled:                true,
		CircuitBreakerForceClosed:            false,
		CircuitBreakerForceOpen:              cb.forceOpen,
		CircuitBreakerErrorThresholdPercent:  50,
		CircuitBreakerSleepWindow:            5000,
		CircuitBreakerRequestVolumeThreshold: 20,
	})
	if err != nil {
		return err
	}
	err = sh.writeToRequests(eventBytes)
	if err != nil {
		return err
	}

	return nil
}

func (sh *StreamHandler) publishThreadPools(pool *executorPool) error {
	now := time.Now()

	eventBytes, err := json.Marshal(&streamThreadPoolMetric{
		Type:           "HystrixThreadPool",
		Name:           pool.Name,
		ReportingHosts: 1,

		CurrentActiveCount:        uint32(pool.ActiveCount()),
		CurrentTaskCount:          0,
		CurrentCompletedTaskCount: 0,

		RollingCountThreadsExecuted: uint32(pool.Metrics.Executed.Sum(now)),
		RollingMaxActiveThreads:     uint32(pool.Metrics.MaxActiveRequests.Max(now)),

		CurrentPoolSize:        uint32(pool.Max),
		CurrentCorePoolSize:    uint32(pool.Max),
		CurrentLargestPoolSize: uint32(pool.Max),
		CurrentMaximumPoolSize: uint32(pool.Max),

		RollingStatsWindow:          10000,
		QueueSizeRejectionThreshold: 0,
		CurrentQueueSize:            0,
	})
	if err != nil {
		return err
	}
	err = sh.writeToRequests(eventBytes)

	return nil
}

func (sh *StreamHandler) writeToRequests(eventBytes []byte) error {
	var b bytes.Buffer
	_, err := b.Write([]byte("data:"))
	if err != nil {
		return err
	}

	_, err = b.Write(eventBytes)
	if err != nil {
		return err
	}
	_, err = b.Write([]byte("\n\n"))
	if err != nil {
		return err
	}
	dataBytes := b.Bytes()
	sh.mu.RLock()

	for _, requestEvents := range sh.requests {
		select {
		case requestEvents <- dataBytes:
		default:
		}
	}
	sh.mu.RUnlock()

	return nil
}

func (sh *StreamHandler) register(req *http.Request) <-chan []byte {
	sh.mu.RLock()
	events, ok := sh.requests[req]
	sh.mu.RUnlock()
	if ok {
		return events
	}

	events = make(chan []byte, streamEventBufferSize)
	sh.mu.Lock()
	sh.requests[req] = events
	sh.mu.Unlock()
	return events
}

func (sh *StreamHandler) unregister(req *http.Request) {
	sh.mu.Lock()
	delete(sh.requests, req)
	sh.mu.Unlock()
}

type streamCmdMetric struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	Group          string `json:"group"`
	Time           int64  `json:"currentTime"`
	ReportingHosts uint32 `json:"reportingHosts"`

	// Health
	RequestCount       uint32 `json:"requestCount"`
	ErrorCount         uint32 `json:"errorCount"`
	ErrorPct           uint32 `json:"errorPercentage"`
	CircuitBreakerOpen bool   `json:"isCircuitBreakerOpen"`

	RollingCountCollapsedRequests  uint32 `json:"rollingCountCollapsedRequests"`
	RollingCountExceptionsThrown   uint32 `json:"rollingCountExceptionsThrown"`
	RollingCountFailure            uint32 `json:"rollingCountFailure"`
	RollingCountFallbackFailure    uint32 `json:"rollingCountFallbackFailure"`
	RollingCountFallbackRejection  uint32 `json:"rollingCountFallbackRejection"`
	RollingCountFallbackSuccess    uint32 `json:"rollingCountFallbackSuccess"`
	RollingCountResponsesFromCache uint32 `json:"rollingCountResponsesFromCache"`
	RollingCountSemaphoreRejected  uint32 `json:"rollingCountSemaphoreRejected"`
	RollingCountShortCircuited     uint32 `json:"rollingCountShortCircuited"`
	RollingCountSuccess            uint32 `json:"rollingCountSuccess"`
	RollingCountThreadPoolRejected uint32 `json:"rollingCountThreadPoolRejected"`
	RollingCountTimeout            uint32 `json:"rollingCountTimeout"`

	CurrentConcurrentExecutionCount uint32 `json:"currentConcurrentExecutionCount"`

	LatencyExecuteMean uint32           `json:"latencyExecute_mean"`
	LatencyExecute     streamCmdLatency `json:"latencyExecute"`
	LatencyTotalMean   uint32           `json:"latencyTotal_mean"`
	LatencyTotal       streamCmdLatency `json:"latencyTotal"`

	// Properties
	CircuitBreakerRequestVolumeThreshold             uint32 `json:"propertyValue_circuitBreakerRequestVolumeThreshold"`
	CircuitBreakerSleepWindow                        uint32 `json:"propertyValue_circuitBreakerSleepWindowInMilliseconds"`
	CircuitBreakerErrorThresholdPercent              uint32 `json:"propertyValue_circuitBreakerErrorThresholdPercentage"`
	CircuitBreakerForceOpen                          bool   `json:"propertyValue_circuitBreakerForceOpen"`
	CircuitBreakerForceClosed                        bool   `json:"propertyValue_circuitBreakerForceClosed"`
	CircuitBreakerEnabled                            bool   `json:"propertyValue_circuitBreakerEnabled"`
	ExecutionIsolationStrategy                       string `json:"propertyValue_executionIsolationStrategy"`
	ExecutionIsolationThreadTimeout                  uint32 `json:"propertyValue_executionIsolationThreadTimeoutInMilliseconds"`
	ExecutionIsolationThreadInterruptOnTimeout       bool   `json:"propertyValue_executionIsolationThreadInterruptOnTimeout"`
	ExecutionIsolationThreadPoolKeyOverride          string `json:"propertyValue_executionIsolationThreadPoolKeyOverride"`
	ExecutionIsolationSemaphoreMaxConcurrentRequests uint32 `json:"propertyValue_executionIsolationSemaphoreMaxConcurrentRequests"`
	FallbackIsolationSemaphoreMaxConcurrentRequests  uint32 `json:"propertyValue_fallbackIsolationSemaphoreMaxConcurrentRequests"`
	RollingStatsWindow                               uint32 `json:"propertyValue_metricsRollingStatisticalWindowInMilliseconds"`
	RequestCacheEnabled                              bool   `json:"propertyValue_requestCacheEnabled"`
	RequestLogEnabled                                bool   `json:"propertyValue_requestLogEnabled"`
}

type streamCmdLatency struct {
	Timing0   uint32 `json:"0"`
	Timing25  uint32 `json:"25"`
	Timing50  uint32 `json:"50"`
	Timing75  uint32 `json:"75"`
	Timing90  uint32 `json:"90"`
	Timing95  uint32 `json:"95"`
	Timing99  uint32 `json:"99"`
	Timing995 uint32 `json:"99.5"`
	Timing100 uint32 `json:"100"`
}

type streamThreadPoolMetric struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	ReportingHosts uint32 `json:"reportingHosts"`

	CurrentActiveCount        uint32 `json:"currentActiveCount"`
	CurrentCompletedTaskCount uint32 `json:"currentCompletedTaskCount"`
	CurrentCorePoolSize       uint32 `json:"currentCorePoolSize"`
	CurrentLargestPoolSize    uint32 `json:"currentLargestPoolSize"`
	CurrentMaximumPoolSize    uint32 `json:"currentMaximumPoolSize"`
	CurrentPoolSize           uint32 `json:"currentPoolSize"`
	CurrentQueueSize          uint32 `json:"currentQueueSize"`
	CurrentTaskCount          uint32 `json:"currentTaskCount"`

	RollingMaxActiveThreads     uint32 `json:"rollingMaxActiveThreads"`
	RollingCountThreadsExecuted uint32 `json:"rollingCountThreadsExecuted"`

	RollingStatsWindow          uint32 `json:"propertyValue_metricsRollingStatisticalWindowInMilliseconds"`
	QueueSizeRejectionThreshold uint32 `json:"propertyValue_queueSizeRejectionThreshold"`
}

func currentTime() int64 {
	return time.Now().UnixNano() / int64(1000000)
}
