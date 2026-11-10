package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const nanoUSD = int64(1_000_000_000)

var providerCaps = map[string]int64{"openai": 300 * nanoUSD, "anthropic": 400 * nanoUSD}
var ErrBudgetBlocked = errors.New("delegation budget unavailable")
var ErrNotSent = errors.New("delegation request was not sent")
var ErrUsagePending = errors.New("canonical delegation usage pending")

// Prices are an immutable reviewed deployment snapshot. Unknown coverage or
// model ceilings cannot be made safe by observing the answer's text length.
type Prices struct {
	Version               string `json:"version"`
	Period                string `json:"period"`
	InputPerMillion       int64  `json:"inputNanoUSDPerMillion"`
	CachedInputPerMillion int64  `json:"cachedInputNanoUSDPerMillion"`
	OutputPerMillion      int64  `json:"outputNanoUSDPerMillion"`
	LimitsCertified       bool   `json:"limitsCertified"`
	BillableUsageCovered  bool   `json:"billableUsageCovered"`
}
type Charges struct {
	Input, CachedInput, Output, Total *int64
	PriceVersion                      string
}

// ExternalSpend excludes this gate's attempts; includes uncontrolled spend and
// holds across all accounts/projects of the billing family, not per key.
type ExternalSpend struct {
	Known           bool  `json:"known"`
	OutsideGateOnly bool  `json:"outsideGateOnly"`
	Nanos           int64 `json:"nanos"`
}

type budgetAttempt struct {
	LabRunID       string
	EverDispatched bool
	Execution      Execution
	Profile        Profile
	ProfileDigest  string
	PayloadHash    string
	ScopeDigest    string
	State          string
	Reserved       int64
	Charged        *int64
	ReceiptDigest  string
	SettledPeriod  string
}
type budgetState struct {
	Grant    *LabGrant `json:"grant,omitempty"`
	Version  int
	Period   string
	External map[string]map[string]ExternalSpend
	Attempts map[string]budgetAttempt
}

// Budget is one daemon-wide journal directory, shared across every project and
// both fixed billing families. File locking also protects separate instances.
type Budget struct {
	OwnerRoot    string
	Dir          string
	Now          func() time.Time
	pending      sync.Map
	writePending func(context.Context, string, []byte) error
}

func (b *Budget) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Budget) update(ctx context.Context, change func(*budgetState) error) error {
	return b.updateGuarded(ctx, change, nil)
}
func (b *Budget) updateGuarded(ctx context.Context, change func(*budgetState) error, guard func(func() error) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(b.Dir, 0700); err != nil {
		return ErrBudgetBlocked
	}
	info, err := os.Lstat(b.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrBudgetBlocked
	}
	lock, err := os.OpenFile(filepath.Join(b.Dir, "budget.lock"), budgetLockFlags, 0600)
	if err != nil {
		return ErrBudgetBlocked
	}
	defer lock.Close()
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ErrBudgetBlocked
	}
	for {
		locked, err := tryBudgetLock(lock)
		if err != nil {
			return ErrBudgetBlocked
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	path := filepath.Join(b.Dir, "budget.json")
	state := budgetState{Version: 1, External: map[string]map[string]ExternalSpend{}, Attempts: map[string]budgetAttempt{}}
	f, err := os.Open(path)
	if err == nil {
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			f.Close()
			return ErrBudgetBlocked
		}
		decoder := json.NewDecoder(io.LimitReader(f, 4<<20))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&state)
		extra := decoder.Decode(new(any))
		closeErr := f.Close()
		if err != nil || extra != io.EOF || closeErr != nil || state.Version != 1 || state.Attempts == nil || state.External == nil {
			return ErrBudgetBlocked
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrBudgetBlocked
	}
	if err := validBudgetState(state); err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > 4<<20 {
		return ErrBudgetBlocked
	}
	persist := func() error { return atomicBudgetFile(ctx, path, data) }
	if guard != nil {
		return guard(persist)
	}
	return persist()
}

func validBudgetState(state budgetState) error {
	if state.Grant != nil && !state.Grant.valid(state.Grant.OwnerRoot) {
		return ErrBudgetBlocked
	}
	for period, providers := range state.External {
		if _, err := time.Parse("2006-01", period); err != nil {
			return ErrBudgetBlocked
		}
		for provider, spend := range providers {
			if providerCaps[provider] == 0 || spend.Nanos < 0 {
				return ErrBudgetBlocked
			}
		}
	}
	for key, a := range state.Attempts {
		if a.LabRunID != "" && (state.Grant == nil || a.LabRunID != state.Grant.RunID || !state.Grant.bound(a.Execution.ProjectID, a.Execution.SessionID) || (a.State != "reserved" && a.State != "released" && !a.EverDispatched) || (a.State == "reserved" && a.EverDispatched)) {
			return ErrBudgetBlocked
		}
		if _, err := time.Parse("2006-01", a.Profile.Prices.Period); err != nil {
			return ErrBudgetBlocked
		}
		worst, worstErr := worstCharge(a.Profile)
		if key != attemptKey(a.Execution, a.Profile) || worstErr != nil || a.Reserved != worst || a.ProfileDigest != a.Profile.digest() || len(a.PayloadHash) != 64 || !validDigest(a.ScopeDigest) || a.Execution.ProjectPolicyRevision == "" {
			return ErrBudgetBlocked
		}
		switch a.State {
		case "reserved", "dispatched", "usage_pending", "unknown", "released":
			if a.Charged != nil {
				return ErrBudgetBlocked
			}
		case "settled":
			if _, err := time.Parse("2006-01", a.SettledPeriod); err != nil || a.SettledPeriod < a.Profile.Prices.Period {
				return ErrBudgetBlocked
			}
			if a.Charged == nil || *a.Charged < 0 || *a.Charged > a.Reserved {
				return ErrBudgetBlocked
			}
		default:
			return ErrBudgetBlocked
		}
	}
	return nil
}
func atomicBudgetFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".budget-")
	if err != nil {
		return ErrBudgetBlocked
	}
	defer os.Remove(f.Name())
	if n, writeErr := f.Write(data); writeErr != nil || n != len(data) {
		f.Close()
		return ErrBudgetBlocked
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return ErrBudgetBlocked
	}
	if err = f.Close(); err != nil {
		return ErrBudgetBlocked
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return ErrBudgetBlocked
	}
	return syncBudgetDirectory(filepath.Dir(path))
}

// ApprovePeriod is an explicit host operation. It never clears open holds or
// resets on a clock/month change, and never reinterprets earlier receipts.
func (b *Budget) ApprovePeriod(ctx context.Context, period string, external map[string]ExternalSpend) error {
	return b.approvePeriod(ctx, period, external, nil)
}
func (b *Budget) approvePeriod(ctx context.Context, period string, external map[string]ExternalSpend, grant *LabGrant) error {
	if period != b.now().Format("2006-01") || len(external) != 2 {
		return ErrBudgetBlocked
	}
	for _, provider := range []string{"openai", "anthropic"} {
		s, ok := external[provider]
		if !ok || s.Nanos < 0 {
			return ErrBudgetBlocked
		}
	}
	return b.update(ctx, func(s *budgetState) error {
		if grant != nil {
			merged, err := mergeLabGrant(s.Grant, grant)
			if err != nil {
				return ErrBudgetBlocked
			}
			s.Grant = merged
		}
		if s.Period != "" && period < s.Period {
			return ErrBudgetBlocked
		}
		if s.Period != period {
			for _, a := range s.Attempts {
				if a.State != "settled" && a.State != "released" {
					return ErrBudgetBlocked
				}
			}
		}
		// Within a period, new outside-gate knowledge cannot erase previously
		// recorded uncontrolled spending/holds.
		if old, ok := s.External[period]; ok {
			for provider, v := range external {
				if v.Nanos < old[provider].Nanos {
					return ErrBudgetBlocked
				}
			}
		}
		copy := map[string]ExternalSpend{}
		for k, v := range external {
			copy[k] = v
		}
		s.External[period] = copy
		s.Period = period
		return nil
	})
}
func attemptKey(e Execution, p Profile) string {
	v, _ := json.Marshal([]string{e.ProjectID, e.SessionID, e.DelegationID, e.AttemptID, p.BillingProvider, p.ID, p.Revision})
	return textHash(string(v))
}
func priceTokens(tokens, price int64) (int64, error) {
	if tokens < 0 || price < 0 {
		return 0, ErrBudgetBlocked
	}
	n := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(price))
	n.Add(n, big.NewInt(999999))
	n.Div(n, big.NewInt(1000000))
	if !n.IsInt64() {
		return 0, ErrBudgetBlocked
	}
	return n.Int64(), nil
}
func addMoney(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrBudgetBlocked
	}
	return a + b, nil
}
func worstCharge(p Profile) (int64, error) {
	price := p.Prices
	if !p.valid() || (!profileToken.MatchString(price.Version) || !safeText(price.Version)) || !price.LimitsCertified || !price.BillableUsageCovered || price.CachedInputPerMillion < 0 || price.CachedInputPerMillion > price.InputPerMillion {
		return 0, ErrBudgetBlocked
	}
	input, err := priceTokens(p.ContextTokens, price.InputPerMillion)
	if err != nil {
		return 0, err
	}
	output, err := priceTokens(p.OutputTokens, price.OutputPerMillion)
	if err != nil {
		return 0, err
	}
	total, err := addMoney(input, output)
	if err != nil {
		return 0, err
	}
	// Splitting OpenAI input into two separately rounded charge components
	// can cost one additional nano-USD over rounding their combined ceiling.
	if p.BillingProvider == "openai" && (price.InputPerMillion%1000000 != 0 || price.CachedInputPerMillion%1000000 != 0) {
		return addMoney(total, 1)
	}
	return total, nil
}

func (b *Budget) reserve(ctx context.Context, e Execution, p Profile, payload []byte, scopeDigest string) (string, error) {
	if !validDigest(scopeDigest) || e.ProjectID == "" || e.SessionID == "" || e.DelegationID == "" || e.AttemptID == "" || e.ProjectPolicyRevision == "" {
		return "", ErrBudgetBlocked
	}
	worst, err := worstCharge(p)
	if err != nil {
		return "", err
	}
	key := attemptKey(e, p)
	err = b.update(ctx, func(s *budgetState) error {
		if s.Period != b.now().Format("2006-01") || p.Prices.Period != s.Period {
			return ErrBudgetBlocked
		}
		if _, ok := s.Attempts[key]; ok {
			return ErrBudgetBlocked
		}

		runID := ""
		if s.Grant != nil {
			runID = s.Grant.RunID
		}
		s.Attempts[key] = budgetAttempt{LabRunID: runID, Execution: e, Profile: p, ProfileDigest: p.digest(), PayloadHash: textHash(string(payload)), ScopeDigest: scopeDigest, State: "reserved", Reserved: worst}
		return checkBudgetCapacity(s, key, b.now(), b.OwnerRoot)
	})
	return key, err
}
func (b *Budget) transition(ctx context.Context, key, from, to string) error {
	return b.update(ctx, func(s *budgetState) error {
		a, ok := s.Attempts[key]
		if !ok || a.State != from {
			return ErrBudgetBlocked
		}
		if to == "dispatched" && (s.Period != b.now().Format("2006-01") || a.Profile.Prices.Period != s.Period) {
			return ErrBudgetBlocked
		}
		if to == "dispatched" {
			if err := checkBudgetCapacity(s, key, b.now(), b.OwnerRoot); err != nil {
				return err
			}
		}
		if to == "dispatched" {
			a.EverDispatched = true
		}
		a.State = to
		s.Attempts[key] = a
		return nil
	})
}
func (b *Budget) releaseUnsent(ctx context.Context, key string) error {
	return b.update(ctx, func(s *budgetState) error {
		a, ok := s.Attempts[key]
		if !ok || (a.State != "reserved" && a.State != "dispatched") {
			return ErrBudgetBlocked
		}
		a.State = "released"
		s.Attempts[key] = a
		return nil
	})
}

func receiptCharges(a budgetAttempt, r Receipt) (Charges, error) {
	p := a.Profile
	price := p.Prices
	if r.UnknownReason != "" || r.Usage.Input == nil || r.Usage.Output == nil || !price.BillableUsageCovered {
		return Charges{}, ErrBudgetBlocked
	}
	if *r.Usage.Input < 0 || *r.Usage.Input > p.ContextTokens || *r.Usage.Output < 0 || *r.Usage.Output > p.OutputTokens {
		return Charges{}, ErrBudgetBlocked
	}
	cost := Charges{PriceVersion: price.Version}
	input, err := priceTokens(*r.Usage.Input, price.InputPerMillion)
	if err != nil {
		return Charges{}, err
	}
	if p.BillingProvider == "openai" && r.Usage.CachedInput != nil {
		cached := *r.Usage.CachedInput
		if cached < 0 || cached > *r.Usage.Input {
			return Charges{}, ErrBudgetBlocked
		}
		fresh, err := priceTokens(*r.Usage.Input-cached, price.InputPerMillion)
		if err != nil {
			return Charges{}, err
		}
		cache, err := priceTokens(cached, price.CachedInputPerMillion)
		if err != nil {
			return Charges{}, err
		}
		cost.Input, cost.CachedInput = &fresh, &cache
		input, err = addMoney(fresh, cache)
		if err != nil {
			return Charges{}, err
		}
	} else if p.BillingProvider == "openai" {
		if price.InputPerMillion != price.CachedInputPerMillion {
			return Charges{}, ErrBudgetBlocked
		}
		if *r.Usage.Input > 0 && price.InputPerMillion%1000000 != 0 {
			input, err = addMoney(input, 1)
			if err != nil {
				return Charges{}, err
			}
		}
	}
	output, err := priceTokens(*r.Usage.Output, price.OutputPerMillion)
	if err != nil {
		return Charges{}, err
	}
	cost.Output = &output
	total, err := addMoney(input, output)
	if err != nil || total > a.Reserved {
		return Charges{}, ErrBudgetBlocked
	}
	cost.Total = &total
	// Empty Anthropic compatibility details never become known cache zero.
	return cost, nil
}

// budgetBridge is the final consumer's sole transport+recorder pair. Admission
// is a short policy revision linearization, never a lock held over provider I/O.
type budgetBridge struct {
	budget       *Budget
	execution    Execution
	profile      Profile
	actual       Transport
	canonical    Recorder
	authorize    func(context.Context) bool
	commitGuard  func(context.Context, func() error) error
	afterReserve func(context.Context)
	key          string
	started      bool
}

func (b *budgetBridge) Do(ctx context.Context, r WireRequest) (WireResponse, error) {
	if b.actual == nil || b.canonical == nil || (b.authorize == nil && b.commitGuard == nil) || r.Provider != b.profile.BillingProvider || r.Endpoint != b.profile.endpoint() || r.Model != b.profile.Model {
		return WireResponse{}, ErrBudgetBlocked
	}
	var body struct {
		Model    string                           `json:"model"`
		Max      int64                            `json:"max_completion_tokens"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	d := json.NewDecoder(bytes.NewReader(r.Payload))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || body.Model != b.profile.Model || body.Max != b.profile.OutputTokens || len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		return WireResponse{}, ErrBudgetBlocked
	}
	key, err := b.budget.reserve(ctx, b.execution, b.profile, r.Payload, r.ScopeDigest)
	if err != nil {
		return WireResponse{}, err
	}
	b.key = key
	if b.afterReserve != nil {
		b.afterReserve(ctx)
	}
	if (b.authorize != nil && !b.authorize(ctx)) || ctx.Err() != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = b.budget.releaseUnsent(releaseCtx, key)
		return WireResponse{}, ErrBudgetBlocked
	}
	dispatch := func() error { return b.budget.transition(ctx, key, "reserved", "dispatched") }
	if b.commitGuard != nil {
		dispatch = func() error {
			return b.budget.updateGuarded(ctx, func(s *budgetState) error {
				a, ok := s.Attempts[key]
				if !ok || a.State != "reserved" || s.Period != b.budget.now().Format("2006-01") || a.Profile.Prices.Period != s.Period {
					return ErrBudgetBlocked
				}
				if err := checkBudgetCapacity(s, key, b.budget.now(), b.budget.OwnerRoot); err != nil {
					return err
				}
				a.EverDispatched = true
				a.State = "dispatched"
				s.Attempts[key] = a
				return nil
			}, func(persist func() error) error { return b.commitGuard(ctx, persist) })
		}
	}
	if err := dispatch(); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = b.budget.releaseUnsent(releaseCtx, key)
		return WireResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = b.budget.releaseUnsent(releaseCtx, key)
		return WireResponse{}, err
	}
	b.started = true
	response, err := b.actual.Do(ctx, r)
	if errors.Is(err, ErrNotSent) {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = b.budget.releaseUnsent(releaseCtx, key)
		b.started = false
	}
	return response, err
}
func (b *budgetBridge) Record(ctx context.Context, r Receipt) error {
	if !b.started {
		return nil
	} // no provider attempt; no invented usage source
	return b.budget.record(ctx, b.key, r, b.canonical)
}
func (b *Budget) record(ctx context.Context, key string, r Receipt, canonical Recorder) error {
	var attempt budgetAttempt
	already := false
	digestBytes, marshalErr := json.Marshal(r)
	if marshalErr != nil {
		return ErrBudgetBlocked
	}
	digest := textHash(string(digestBytes))
	err := b.update(ctx, func(s *budgetState) error {
		a, ok := s.Attempts[key]
		if !ok || a.Execution != r.Execution || a.Profile.ID != r.ProfileID || a.Profile.Revision != r.ProfileRevision || a.Profile.digest() != r.ProfileDigest || a.Profile.BillingProvider != r.Provider || a.Profile.Model != r.Model || (a.ReceiptDigest != "" && a.ReceiptDigest != digest) {
			return ErrBudgetBlocked
		}
		if a.State == "settled" || a.State == "unknown" {
			already = true
			return nil
		}
		if a.State != "dispatched" && a.State != "usage_pending" {
			return ErrBudgetBlocked
		}
		attempt = a
		b.pending.Store(key, r)
		// Pending receipt is a bounded safe projection, not a second token ledger.
		writer := b.writePending
		if writer == nil {
			writer = atomicBudgetFile
		}
		if err := writer(ctx, filepath.Join(b.Dir, key+".pending"), digestBytes); err != nil {
			return err
		}
		a.ReceiptDigest = digest
		a.State = "usage_pending"
		s.Attempts[key] = a
		return nil
	})
	if already {
		b.pending.Delete(key)
	}
	if err != nil || already {
		return err
	}
	charges, chargeErr := receiptCharges(attempt, r)
	r.Charges = charges
	if err := canonical.Record(ctx, r); err != nil {
		return ErrUsagePending
	}
	err = b.update(ctx, func(s *budgetState) error {
		a := s.Attempts[key]
		if a.State != "usage_pending" || a.ReceiptDigest != digest {
			return ErrBudgetBlocked
		}
		if chargeErr != nil {
			a.State = "unknown"
		} else {
			a.State = "settled"
			a.SettledPeriod = b.now().Format("2006-01")
			a.Charged = charges.Total
		}
		s.Attempts[key] = a
		return nil
	})
	if err != nil {
		return err
	}
	b.pending.Delete(key)
	_ = os.Remove(filepath.Join(b.Dir, key+".pending"))
	if chargeErr != nil {
		return ErrBudgetBlocked
	}
	return nil
}

// ReplayReceipt retries only the trusted canonical receipt write, never the
// provider request. The persisted attempt's descriptor supplies the old price.
func (b *Budget) ReplayReceipt(ctx context.Context, key string, canonical Recorder) error {
	if len(key) != 64 || key != filepath.Base(key) || canonical == nil {
		return ErrBudgetBlocked
	}
	if value, ok := b.pending.Load(key); ok {
		return b.record(ctx, key, value.(Receipt), canonical)
	}
	f, err := os.Open(filepath.Join(b.Dir, key+".pending"))
	if err != nil {
		return ErrBudgetBlocked
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 65536))
	d.DisallowUnknownFields()
	var r Receipt
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return ErrBudgetBlocked
	}
	return b.record(ctx, key, r, canonical)
}

// checkBudgetCapacity runs under the budget file lock for reserve and final
// dispatch. The current reservation is included exactly once; any other open
// attempt freezes admission, and changed outside-gate knowledge is authoritative.
func checkBudgetCapacity(s *budgetState, reservedKey string, now time.Time, root string) error {
	totals := map[string]int64{}
	for _, provider := range []string{"openai", "anthropic"} {
		external, ok := s.External[s.Period][provider]
		if !ok || !external.Known || !external.OutsideGateOnly || external.Nanos < 0 {
			return ErrBudgetBlocked
		}
		totals[provider] = external.Nanos
	}
	for key, a := range s.Attempts {
		if a.State == "released" {
			continue
		}
		var amount int64
		if a.State == "settled" {
			if a.Profile.Prices.Period > s.Period || a.SettledPeriod < s.Period {
				continue
			}
			if a.Charged == nil {
				return ErrBudgetBlocked
			}
			amount = *a.Charged
		} else if key == reservedKey && a.State == "reserved" {
			amount = a.Reserved
		} else {
			return ErrBudgetBlocked
		}
		next, err := addMoney(totals[a.Profile.BillingProvider], amount)
		if err != nil {
			return err
		}
		totals[a.Profile.BillingProvider] = next
	}
	for provider, total := range totals {
		cap, ok := providerCaps[provider]
		if !ok || total > cap {
			return ErrBudgetBlocked
		}
	}
	total, err := addMoney(totals["openai"], totals["anthropic"])
	if err != nil || total > 630*nanoUSD {
		return ErrBudgetBlocked
	}
	return checkLabCapacity(s, reservedKey, now, root)
}
