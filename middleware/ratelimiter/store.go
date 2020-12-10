package ratelimiter

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/System-Glitch/goyave/v3"
)

type limiter struct {
	config   Config
	counter  int
	resetsAt time.Time
	mx       sync.Mutex
}

func newLimiter(config Config) *limiter {
	return &limiter{
		config:   config,
		counter:  0,
		resetsAt: time.Now().Add(config.QuotaDuration),
	}
}

func (l *limiter) validateAndUpdate(response *goyave.Response) bool {

	l.mx.Lock()
	defer l.mx.Unlock()

	valid := !l.hasExceededRequestQuota()
	l.counter++
	l.updateResponseHeaders(response)
	return valid
}

func (l *limiter) updateResponseHeaders(response *goyave.Response) {
	response.Header().Set(
		"RateLimit-Limit",
		fmt.Sprintf("%v, %v;w=%v", l.config.RequestQuota, l.config.RequestQuota, l.config.QuotaDuration.Seconds()),
	)

	response.Header().Set(
		"RateLimit-Remaining",
		fmt.Sprintf("%v", l.getRemainingRequestQuota()),
	)

	response.Header().Set(
		"RateLimit-Reset",
		fmt.Sprintf("%v", l.getSecondsToQuotaReset()),
	)
}

func (l *limiter) hasExceededRequestQuota() bool {
	return l.counter >= l.config.RequestQuota
}

func (l *limiter) getRemainingRequestQuota() int {
	return l.config.RequestQuota - l.counter
}

func (l *limiter) getSecondsToQuotaReset() float64 {
	return -math.Round(time.Since(l.resetsAt).Seconds())
}

type limiterStore struct {
	mx    sync.RWMutex
	store map[interface{}]*limiter
}

func newLimiterStore() limiterStore {
	return limiterStore{
		store: make(map[interface{}]*limiter),
	}
}

func (ls *limiterStore) set(key interface{}, limiter *limiter) {
	ls.mx.Lock()
	defer ls.mx.Unlock()
	ls.store[key] = limiter

	// Remove expired entries from the map to avoid store map growing too much
	// Warning though, go maps aren't shrunk after key deletion,
	// see https://github.com/golang/go/issues/20135
	time.AfterFunc(limiter.config.QuotaDuration, func() {
		ls.mx.Lock()
		defer ls.mx.Unlock()
		delete(ls.store, key)
	})
}

func (ls *limiterStore) get(key interface{}) *limiter {
	ls.mx.RLock()
	defer ls.mx.RUnlock()
	return ls.store[key]
}
