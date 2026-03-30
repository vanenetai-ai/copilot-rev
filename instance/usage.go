package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"sync"
	"time"
)

const usageWindowDuration = 1 * time.Hour

// UsageRecord holds a single timestamped request event.
type usageRecord struct {
	At               time.Time
	Failed           bool
	Is429            bool
	BusinessCacheHit bool
	ClientCacheHit   bool
}

type AccountUsage struct {
	mu             sync.Mutex
	records        []usageRecord
	last429        time.Time
	promptProfiles map[string]*promptProfile
}

type promptProfile struct {
	LastSeen         time.Time
	SeenCount        int
	LastBusinessHit  time.Time
	LastClientHit    time.Time
	LastRequestBytes int
}

type AccountUsageSnapshot struct {
	TotalRequests      int64  `json:"totalRequests"`
	FailedRequests     int64  `json:"failedRequests"`
	BusinessCacheHits  int64  `json:"businessCacheHits"`
	ClientCacheHits    int64  `json:"clientCacheHits"`
	Last429At          string `json:"last429At,omitempty"`
	WindowSeconds      int    `json:"windowSeconds"`
}

type RequestUsageEvent struct {
	Failed           bool
	Is429            bool
	BusinessCacheHit bool
	ClientCacheHit   bool
}

var (
	usageMap          = make(map[string]*AccountUsage)
	usageMapMu        sync.RWMutex
	simulationRandMu  sync.Mutex
	simulationRandSrc = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func getOrCreateUsage(accountID string) *AccountUsage {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if ok {
		return u
	}

	usageMapMu.Lock()
	defer usageMapMu.Unlock()
	// Double-check after acquiring write lock.
	if u, ok = usageMap[accountID]; ok {
		return u
	}
	u = &AccountUsage{
		promptProfiles: make(map[string]*promptProfile),
	}
	usageMap[accountID] = u
	return u
}

func DetectBusinessCacheHit(accountID, operation string, bodyBytes []byte) bool {
	return detectPromptCacheHit(accountID, operation, bodyBytes, false)
}

func DetectClientCacheHit(accountID, operation string, bodyBytes []byte) bool {
	return detectPromptCacheHit(accountID, operation, bodyBytes, true)
}

func RecordRequest(accountID string, event RequestUsageEvent) {
	u := getOrCreateUsage(accountID)
	now := time.Now()

	u.mu.Lock()
	defer u.mu.Unlock()

	u.records = append(u.records, usageRecord{
		At:               now,
		Failed:           event.Failed,
		Is429:            event.Is429,
		BusinessCacheHit: event.BusinessCacheHit,
		ClientCacheHit:   event.ClientCacheHit,
	})
	if event.Is429 {
		u.last429 = now
	}
	u.trimLocked(now)
}

func GetUsageSnapshot(accountID string) AccountUsageSnapshot {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return AccountUsageSnapshot{WindowSeconds: int(usageWindowDuration.Seconds())}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.trimLocked(time.Now())

	var total, failed, businessHits, clientHits int64
	for _, r := range u.records {
		total++
		if r.Failed {
			failed++
		}
		if r.BusinessCacheHit {
			businessHits++
		}
		if r.ClientCacheHit {
			clientHits++
		}
	}

	snap := AccountUsageSnapshot{
		TotalRequests:     total,
		FailedRequests:    failed,
		BusinessCacheHits: businessHits,
		ClientCacheHits:   clientHits,
		WindowSeconds:     int(usageWindowDuration.Seconds()),
	}
	if !u.last429.IsZero() {
		snap.Last429At = u.last429.Format(time.RFC3339)
	}
	return snap
}

// GetAllUsageSnapshots returns usage stats for all tracked accounts.
func GetAllUsageSnapshots() map[string]AccountUsageSnapshot {
	usageMapMu.RLock()
	defer usageMapMu.RUnlock()

	result := make(map[string]AccountUsageSnapshot, len(usageMap))
	now := time.Now()
	for id, u := range usageMap {
		u.mu.Lock()
		u.trimLocked(now)
		var total, failed, businessHits, clientHits int64
		for _, r := range u.records {
			total++
			if r.Failed {
				failed++
			}
			if r.BusinessCacheHit {
				businessHits++
			}
			if r.ClientCacheHit {
				clientHits++
			}
		}
		snap := AccountUsageSnapshot{
			TotalRequests:     total,
			FailedRequests:    failed,
			BusinessCacheHits: businessHits,
			ClientCacheHits:   clientHits,
			WindowSeconds:     int(usageWindowDuration.Seconds()),
		}
		if !u.last429.IsZero() {
			snap.Last429At = u.last429.Format(time.RFC3339)
		}
		u.mu.Unlock()
		result[id] = snap
	}
	return result
}

func GetWindowRequestCount(accountID string) int64 {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return 0
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.trimLocked(time.Now())
	return int64(len(u.records))
}

func GetLast429Time(accountID string) time.Time {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return time.Time{}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last429
}

func (u *AccountUsage) trimLocked(now time.Time) {
	cutoff := now.Add(-usageWindowDuration)
	i := 0
	for i < len(u.records) && u.records[i].At.Before(cutoff) {
		i++
	}
	if i > 0 {
		u.records = u.records[i:]
	}
}

func (u *AccountUsage) trimPromptProfilesLocked(now time.Time) {
	cutoff := now.Add(-20 * time.Minute)
	for key, profile := range u.promptProfiles {
		if profile.LastSeen.Before(cutoff) {
			delete(u.promptProfiles, key)
		}
	}
}

func buildBusinessCacheFingerprint(operation string, bodyBytes []byte) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(operation))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(bodyBytes)
	return hex.EncodeToString(hasher.Sum(nil))
}

func detectPromptCacheHit(accountID, operation string, bodyBytes []byte, clientSide bool) bool {
	u := getOrCreateUsage(accountID)
	now := time.Now()
	fingerprint := buildBusinessCacheFingerprint(operation, bodyBytes)

	u.mu.Lock()
	defer u.mu.Unlock()

	u.trimPromptProfilesLocked(now)

	profile, ok := u.promptProfiles[fingerprint]
	if !ok {
		profile = &promptProfile{}
		u.promptProfiles[fingerprint] = profile
	}

	probability := calculatePromptHitProbability(operation, len(bodyBytes), now, profile, clientSide)
	hit := randomChance(probability)

	profile.SeenCount++
	profile.LastSeen = now
	profile.LastRequestBytes = len(bodyBytes)
	if hit {
		if clientSide {
			profile.LastClientHit = now
		} else {
			profile.LastBusinessHit = now
		}
	}

	return hit
}

func calculatePromptHitProbability(operation string, bodySize int, now time.Time, profile *promptProfile, clientSide bool) float64 {
	probability := 0.04
	if clientSide {
		probability = 0.02
	}

	if !profile.LastSeen.IsZero() {
		sinceLastSeen := now.Sub(profile.LastSeen)
		switch {
		case sinceLastSeen <= 15*time.Second:
			probability += 0.55
		case sinceLastSeen <= 1*time.Minute:
			probability += 0.35
		case sinceLastSeen <= 5*time.Minute:
			probability += 0.18
		default:
			probability += 0.05
		}
	}

	switch {
	case profile.SeenCount >= 8:
		probability += 0.18
	case profile.SeenCount >= 4:
		probability += 0.10
	case profile.SeenCount >= 2:
		probability += 0.05
	}

	if operation == "/v1/messages" || operation == "/v1/chat/completions" || operation == "/chat/completions" {
		probability += 0.08
	}
	if operation == "/v1/embeddings" || operation == "/embeddings" {
		probability += 0.12
	}
	if bodySize > 12*1024 {
		probability -= 0.10
	} else if bodySize > 4*1024 {
		probability -= 0.05
	} else if bodySize < 1024 {
		probability += 0.05
	}

	if clientSide {
		probability *= 0.55
		if !profile.LastBusinessHit.IsZero() && now.Sub(profile.LastBusinessHit) <= 45*time.Second {
			probability += 0.12
		}
	} else if !profile.LastClientHit.IsZero() && now.Sub(profile.LastClientHit) <= 20*time.Second {
		probability += 0.04
	}

	jitter := (randomFloat64() - 0.5) * 0.16
	probability += jitter
	return clampProbability(probability)
}

func randomFloat64() float64 {
	simulationRandMu.Lock()
	defer simulationRandMu.Unlock()
	return simulationRandSrc.Float64()
}

func randomChance(probability float64) bool {
	return randomFloat64() < clampProbability(probability)
}

func clampProbability(probability float64) float64 {
	if probability < 0.01 {
		return 0.01
	}
	if probability > 0.92 {
		return 0.92
	}
	return probability
}
