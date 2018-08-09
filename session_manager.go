package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/TykTechnologies/leakybucket"
	"github.com/TykTechnologies/leakybucket/memorycache"
	"github.com/TykTechnologies/tyk/config"
	"github.com/TykTechnologies/tyk/storage"
	"github.com/TykTechnologies/tyk/user"
)

type PublicSession struct {
	Quota struct {
		QuotaMax       int64 `json:"quota_max"`
		QuotaRemaining int64 `json:"quota_remaining"`
		QuotaRenews    int64 `json:"quota_renews"`
	} `json:"quota"`
	RateLimit struct {
		Rate float64 `json:"requests"`
		Per  float64 `json:"per_unit"`
	} `json:"rate_limit"`
}

const (
	QuotaKeyPrefix     = "quota-"
	RateLimitKeyPrefix = "rate-limit-"

	quotaChanBufferSize = 256
)

type keyQuota struct {
	key        string
	session    *user.SessionState
	isExceeded bool
	reqChan    chan bool
}

// SessionLimiter is the rate limiter for the API, use ForwardMessage() to
// check if a message should pass through or not
type SessionLimiter struct {
	bucketStore leakybucket.Storage

	keyQuotas   map[string]keyQuota
	keyQuotasMu sync.Mutex
}

func (l *SessionLimiter) getKeyQuota(key string, session *user.SessionState, store storage.Handler, globalConf *config.Config) keyQuota {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()

	// check if key quota counter is already running
	if quota, ok := l.keyQuotas[key]; ok {
		return quota
	}

	// create and start new key quota counter
	quota := keyQuota{
		key:     key,
		session: session,
		reqChan: make(chan bool, quotaChanBufferSize),
	}
	l.keyQuotas[key] = quota
	syncEvery := time.Duration(session.QuotaRenewalRate*1000/int64(globalConf.DistributedQuotaSyncFrequency)) * time.Millisecond
	go l.startCounter(quota, store, syncEvery)

	return quota
}

func (l *SessionLimiter) setQuotaIsExceeded(key string, isExceeded bool) {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()
	if quota, ok := l.keyQuotas[key]; ok {
		quota.isExceeded = isExceeded
		l.keyQuotas[key] = quota
	}
}

func (l *SessionLimiter) startCounter(quota keyQuota, store storage.Handler, syncEvery time.Duration) {
	// initialize total counter with current value from centralized Redis storage
	// by supplying 0 increment
	totalCounter := store.IncrementByWithExpire(quota.key, 0, quota.session.QuotaRenewalRate)

	// this var will be used to count requests per aggregation period
	var localCounter int64

	for {
		select {
		case _, ok := <-quota.reqChan:
			// check if channel was closed and it is time to stop go-routine
			if !ok {
				return
			}

			// check if quota was exceeded
			if totalCounter >= quota.session.QuotaMax {
				l.setQuotaIsExceeded(quota.key, true)
				localCounter = 0
				continue
			}

			// increment last known total counter and local aggregated counter
			totalCounter++
			localCounter++

			// update remaining
			remaining := quota.session.QuotaMax - totalCounter
			if remaining < 0 {
				quota.session.QuotaRemaining = 0
			} else {
				quota.session.QuotaRemaining = remaining
			}

		case <-time.After(syncEvery):
			// aggregation period is done
			// overwrite totalCounter from centralized Redis storage
			totalCounter = store.IncrementByWithExpire(quota.key, localCounter, quota.session.QuotaRenewalRate)
			// reset
			localCounter = 0
			l.setQuotaIsExceeded(quota.key, false)
		}
	}
}

func (l *SessionLimiter) stopCounters() {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()
	for _, quota := range l.keyQuotas {
		close(quota.reqChan)
	}
}

func (l *SessionLimiter) doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey string, currentSession *user.SessionState, store storage.Handler, globalConf *config.Config) bool {
	log.Debug("[RATELIMIT] Inbound raw key is: ", key)
	log.Debug("[RATELIMIT] Rate limiter key is: ", rateLimiterKey)
	pipeline := globalConf.EnableNonTransactionalRateLimiter
	ratePerPeriodNow, _ := store.SetRollingWindow(rateLimiterKey, int64(currentSession.Per), "-1", pipeline)

	//log.Info("Num Requests: ", ratePerPeriodNow)

	// Subtract by 1 because of the delayed add in the window
	subtractor := 1
	if globalConf.EnableSentinelRateLImiter {
		// and another subtraction because of the preemptive limit
		subtractor = 2
	}

	//log.Info("break: ", (int(currentSession.Rate) - subtractor))
	if ratePerPeriodNow > int(currentSession.Rate)-subtractor {
		// Set a sentinel value with expire
		if globalConf.EnableSentinelRateLImiter {
			store.SetRawKey(rateLimiterSentinelKey, "1", int64(currentSession.Per))
		}
		return true
	}

	return false
}

type sessionFailReason uint

const (
	sessionFailNone sessionFailReason = iota
	sessionFailRateLimit
	sessionFailQuota
)

// ForwardMessage will enforce rate limiting, returning a non-zero
// sessionFailReason if session limits have been exceeded.
// Key values to manage rate are Rate and Per, e.g. Rate of 10 messages
// Per 10 seconds
func (l *SessionLimiter) ForwardMessage(r *http.Request, currentSession *user.SessionState, key string, store storage.Handler, enableRL, enableQ bool, globalConf *config.Config) sessionFailReason {
	if enableRL {
		if globalConf.EnableSentinelRateLImiter {
			rateLimiterKey := RateLimitKeyPrefix + currentSession.KeyHash()
			rateLimiterSentinelKey := RateLimitKeyPrefix + currentSession.KeyHash() + ".BLOCKED"

			go l.doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey, currentSession, store, globalConf)

			// Check sentinel
			_, sentinelActive := store.GetRawKey(rateLimiterSentinelKey)
			if sentinelActive == nil {
				// Sentinel is set, fail
				return sessionFailRateLimit
			}
		} else if globalConf.EnableRedisRollingLimiter {
			rateLimiterKey := RateLimitKeyPrefix + currentSession.KeyHash()
			rateLimiterSentinelKey := RateLimitKeyPrefix + currentSession.KeyHash() + ".BLOCKED"

			if l.doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey, currentSession, store, globalConf) {
				return sessionFailRateLimit
			}
		} else {
			// In-memory limiter
			if l.bucketStore == nil {
				l.bucketStore = memorycache.New()
			}

			// If a token has been updated, we must ensure we dont use
			// an old bucket an let the cache deal with it
			bucketKey := key + ":" + currentSession.LastUpdated

			// DRL will always overflow with more servers on low rates
			rate := uint(currentSession.Rate * float64(DRLManager.RequestTokenValue))
			if rate < uint(DRLManager.CurrentTokenValue) {
				rate = uint(DRLManager.CurrentTokenValue)
			}

			userBucket, err := l.bucketStore.Create(bucketKey,
				rate,
				time.Duration(currentSession.Per)*time.Second)
			if err != nil {
				log.Error("Failed to create bucket!")
				return sessionFailRateLimit
			}

			_, errF := userBucket.Add(uint(DRLManager.CurrentTokenValue))

			if errF != nil {
				return sessionFailRateLimit
			}
		}
	}

	if enableQ {
		if globalConf.LegacyEnableAllowanceCountdown {
			currentSession.Allowance--
		}

		if globalConf.DistributedQuotaEnabled {
			if l.DistributedRedisQuotaExceeded(currentSession, key, store, globalConf) {
				return sessionFailQuota
			}
		} else if l.RedisQuotaExceeded(r, currentSession, key, store) {
			return sessionFailQuota
		}
	}

	return sessionFailNone

}

func (l *SessionLimiter) RedisQuotaExceeded(r *http.Request, currentSession *user.SessionState, key string, store storage.Handler) bool {
	// Are they unlimited?
	if currentSession.QuotaMax == -1 {
		// No quota set
		return false
	}

	// Create the key
	log.Debug("[QUOTA] Inbound raw key is: ", key)
	rawKey := QuotaKeyPrefix + currentSession.KeyHash()
	log.Debug("[QUOTA] Quota limiter key is: ", rawKey)
	log.Debug("Renewing with TTL: ", currentSession.QuotaRenewalRate)
	// INCR the key (If it equals 1 - set EXPIRE)
	qInt := store.IncrememntWithExpire(rawKey, currentSession.QuotaRenewalRate)

	// if the returned val is >= quota: block
	if qInt-1 >= currentSession.QuotaMax {
		renewalDate := time.Unix(currentSession.QuotaRenews, 0)
		log.Debug("Renewal Date is: ", renewalDate)
		log.Debug("As epoch: ", currentSession.QuotaRenews)
		log.Debug("Session: ", currentSession)
		log.Debug("Now:", time.Now())
		if time.Now().After(renewalDate) {
			// The renewal date is in the past, we should update the quota!
			// Also, this fixes legacy issues where there is no TTL on quota buckets
			log.Warning("Incorrect key expiry setting detected, correcting")
			go store.DeleteRawKey(rawKey)
			qInt = 1
		} else {
			// Renewal date is in the future and the quota is exceeded
			return true
		}

	}

	// If this is a new Quota period, ensure we let the end user know
	if qInt == 1 {
		current := time.Now().Unix()
		currentSession.QuotaRenews = current + currentSession.QuotaRenewalRate
		ctxScheduleSessionUpdate(r)
	}

	// If not, pass and set the values of the session to quotamax - counter
	remaining := currentSession.QuotaMax - qInt

	if remaining < 0 {
		currentSession.QuotaRemaining = 0
	} else {
		currentSession.QuotaRemaining = remaining
	}

	return false
}

func (l *SessionLimiter) DistributedRedisQuotaExceeded(currentSession *user.SessionState, key string, store storage.Handler, globalConf *config.Config) bool {
	// Are they unlimited?
	if currentSession.QuotaMax == -1 {
		// No quota set
		return false
	}

	rawKey := QuotaKeyPrefix + currentSession.KeyHash()
	quotaCounter := l.getKeyQuota(rawKey, currentSession, store, globalConf)

	// count request if quota is not exceeded for the given key
	if !quotaCounter.isExceeded {
		quotaCounter.reqChan <- true
		return false
	}

	return true
}
