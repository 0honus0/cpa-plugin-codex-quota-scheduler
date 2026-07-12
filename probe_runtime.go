package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type probeReadPayload struct {
	Binding RuntimeBinding
	Windows []ProbeWindowKind
}
type probeSendPayload struct {
	Binding     RuntimeBinding
	Credentials CodexCredentials
	Windows     []ProbeWindowKind
	AttemptID   string
}
type probeReadResult struct {
	Quota       ParsedQuota
	Credentials CodexCredentials
}

func (r *QuotaRefresher) initProbeRuntime() error {
	if r.runtimeStore == nil {
		return nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	r.probeController = NewProbeController(r.now())
	r.probeController.Load(state.ProbeWindows)
	r.probeWAL = NewProbeWAL(r.runtimeStore)
	r.probeFence = NewFenceAllocator(r.runtimeStore, state, nil)
	r.coordinator.opts.AllocateReadSeq = r.probeFence.Next
	return r.bootstrapProbeWindows()
}

func (r *QuotaRefresher) bootstrapProbeWindows() error {
	if r.probeController == nil || !r.state.Config().EnableResetProbe {
		return nil
	}
	now := r.now()
	for _, a := range r.state.Snapshot(now).Accounts {
		b, ok := r.bindings.Lookup(a.AuthID)
		if !ok {
			continue
		}
		for _, pair := range []struct {
			k ProbeWindowKind
			w *QuotaWindow
		}{{ProbeWindowFiveHour, a.Quota.FiveHour}, {ProbeWindowLong, a.Quota.LongWindow}} {
			if pair.w == nil {
				continue
			}
			if _, ok := r.probeController.Window(b.Instance, pair.k); ok {
				continue
			}
			usage := 0.0
			if pair.w.UsedPercent != nil {
				usage = *pair.w.UsedPercent
			}
			base := ResetProbeBaseline(pair.w.ResetAt, usage, 0)
			state := ProbeWaitingReset
			deadline := pair.w.ResetAt.Add(probeRefreshAfterResetDelay)
			if !deadline.After(now) {
				state = ProbePendingCheck
				deadline = now
			}
			if r.runtimeRoster().Capability != CapabilityA {
				state = ProbeWaitingRoster
				deadline = time.Time{}
			}
			r.probeController.SetWindow(b.Instance, pair.k, ProbeWindow{State: state, Baseline: base, Deadline: deadline})
		}
	}
	return r.persistProbeWindows()
}
func (r *QuotaRefresher) persistProbeWindows() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	snapshot := r.probeController.Snapshot()
	_, err := r.runtimeStore.Update(func(s *PersistentState) error { s.ProbeWindows = snapshot; return nil })
	return err
}

func (r *QuotaRefresher) RunProbeDueOnce(ctx context.Context) error {
	if r.probeController == nil {
		return errors.New("probe runtime unavailable")
	}
	if r.runtimeRoster().Capability != CapabilityA {
		return ErrCapabilityB
	}
	if err := r.bootstrapProbeWindows(); err != nil {
		return err
	}
	now := r.now()
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	r.bindings.mu.RLock()
	bindings := make(map[string]RuntimeBinding, len(r.bindings.bindings))
	for id, b := range r.bindings.bindings {
		bindings[id] = b
	}
	r.bindings.mu.RUnlock()
	for authID, b := range bindings {
		if attempt, ok := persisted.ProbeAttempts[b.Instance]; ok && !attempt.SuppressUntil.IsZero() && now.Before(attempt.SuppressUntil) {
			continue
		}
		if b.AuthBlocked {
			for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
				if w, ok := r.probeController.Window(b.Instance, k); ok {
					w.State = ProbeAuthBlocked
					w.AuthBlockedAtLogin = b.Login
					r.probeController.SetWindow(b.Instance, k, w)
				}
			}
			continue
		}
		for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			if w, ok := r.probeController.Window(b.Instance, k); ok && w.State == ProbeAuthBlocked && b.Login > w.AuthBlockedAtLogin {
				w.State = ProbePendingCheck
				w.Deadline = now
				r.probeController.SetWindow(b.Instance, k, w)
			}
		}
		var due []ProbeWindowKind
		for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			if w, ok := r.probeController.Window(b.Instance, k); ok && (w.State == ProbePendingCheck || (!w.Deadline.IsZero() && !w.Deadline.After(now))) {
				due = append(due, k)
			}
		}
		if len(due) == 0 {
			continue
		}
		pre := r.coordinator.SubmitTyped(Intent{AuthID: authID, Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationProbePrecheck, Source: SourceProbePrecheck, Token: b.ExecutionToken(0), Login: b.Login, Fingerprint: b.Fingerprint, Payload: probeReadPayload{Binding: b, Windows: due}}).Await(ctx)
		if pre.Err != nil {
			return pre.Err
		}
		rr := pre.Value.(probeReadResult)
		snaps := probeSnapshots(rr.Quota)
		r.probeController.Advance(b.Instance, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: snaps})
		var lazy []ProbeWindowKind
		for _, k := range due {
			if w, ok := r.probeController.Window(b.Instance, k); ok && w.State == ProbeSentAwaitingVerify {
				lazy = append(lazy, k)
			}
		}
		if len(lazy) == 0 {
			if err := r.persistProbeWindows(); err != nil {
				return err
			}
			continue
		}
		attemptID := fmt.Sprintf("probe-%d-%d", b.Instance, now.UnixNano())
		for _, k := range lazy {
			w, _ := r.probeController.Window(b.Instance, k)
			w.AttemptID = attemptID
			r.probeController.SetWindow(b.Instance, k, w)
		}
		send := r.coordinator.SubmitTyped(Intent{AuthID: authID, Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationProbeSend, Source: SourceProbeActivation, AttemptID: attemptID, Token: b.ExecutionToken(0), Login: b.Login, Fingerprint: b.Fingerprint, Payload: probeSendPayload{Binding: b, Credentials: rr.Credentials, Windows: lazy, AttemptID: attemptID}}).Await(ctx)
		if send.Err != nil {
			return send.Err
		}
		fence := send.Value.(uint64)
		verify := r.coordinator.SubmitTyped(Intent{AuthID: authID, Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationProbeVerify, Source: SourceProbeVerify, StartedAfter: fence, AttemptID: attemptID, Token: b.ExecutionToken(fence), Login: b.Login, Fingerprint: b.Fingerprint, Payload: probeReadPayload{Binding: b, Windows: lazy}}).Await(ctx)
		if verify.Err != nil {
			return verify.Err
		}
		vr := verify.Value.(probeReadResult)
		r.probeController.Advance(b.Instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(vr.Quota)})
		persistedAfter, _ := r.runtimeStore.PersistentSnapshot()
		attempt := persistedAfter.ProbeAttempts[b.Instance]
		for _, k := range lazy {
			if w, ok := r.probeController.Window(b.Instance, k); ok && w.State == ProbeRetryWait && w.Deadline.Before(attempt.SuppressUntil) {
				w.Deadline = attempt.SuppressUntil
				r.probeController.SetWindow(b.Instance, k, w)
			}
		}
		if err := r.probeWAL.Complete(b.Instance); err != nil {
			return err
		}
		if err := r.persistProbeWindows(); err != nil {
			return err
		}
	}
	return nil
}

func (r *QuotaRefresher) RunProbeRecoveryOnce(ctx context.Context) error {
	if r.probeWAL == nil || r.probeController == nil {
		return nil
	}
	for _, intent := range r.probeWAL.Recover(r.now()) {
		var authID string
		var binding RuntimeBinding
		r.bindings.mu.RLock()
		for id, b := range r.bindings.bindings {
			if b.Instance == intent.Instance {
				authID, binding = id, b
				break
			}
		}
		r.bindings.mu.RUnlock()
		if authID == "" || binding.AuthBlocked {
			continue
		}
		windows, _ := intent.Payload.([]ProbeWindowKind)
		intent.AuthID = authID
		intent.Generation = TierGeneration(binding.Generation)
		intent.Class = OperationProbeVerify
		intent.Source = SourceProbeVerify
		intent.AttemptID = fmt.Sprintf("recovery-%d", intent.Instance)
		intent.Token = binding.ExecutionToken(intent.StartedAfter)
		intent.Login = binding.Login
		intent.Fingerprint = binding.Fingerprint
		intent.Payload = probeReadPayload{Binding: binding, Windows: windows}
		result := r.coordinator.SubmitTyped(intent).Await(ctx)
		if result.Err != nil {
			return result.Err
		}
		read := result.Value.(probeReadResult)
		r.probeController.Advance(intent.Instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(read.Quota)})
		state, _ := r.runtimeStore.PersistentSnapshot()
		attempt := state.ProbeAttempts[intent.Instance]
		for _, k := range windows {
			if w, ok := r.probeController.Window(intent.Instance, k); ok && w.State == ProbeRetryWait && w.Deadline.Before(attempt.SuppressUntil) {
				w.Deadline = attempt.SuppressUntil
				r.probeController.SetWindow(intent.Instance, k, w)
			}
		}
		if err := r.probeWAL.Complete(intent.Instance); err != nil {
			return err
		}
	}
	return r.persistProbeWindows()
}

func (r *QuotaRefresher) runTypedHeld(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
	res := OperationResult{Token: intent.Token, Login: intent.Login, Fingerprint: intent.Fingerprint}
	switch intent.Class {
	case OperationQuotaRead, OperationProbePrecheck, OperationProbeVerify:
		p := intent.Payload.(probeReadPayload)
		var auth pluginapi.HostAuthGetResponse
		err := held.DoHTTP(ctx, func(context.Context) error { var e error; auth, e = r.host.GetAuth(p.Binding.AuthIndex); return e })
		if err != nil {
			res.Err = err
			return res
		}
		credentials, err := ExtractCodexCredentials(auth.JSON)
		if err != nil {
			res.Err = err
			return res
		}
		quota, err := r.typedFetchQuota(ctx, held, credentials)
		if err != nil {
			if status, ok := err.(quotaStatusError); ok && status.status == http.StatusUnauthorized && r.bindings != nil {
				_ = r.bindings.MarkAuthBlocked(intent.AuthID)
			}
			res.Err = err
			return res
		}
		res.Value = probeReadResult{Quota: quota, Credentials: credentials}
	case OperationProbeSend:
		p := intent.Payload.(probeSendPayload)
		fence, err := r.probeFence.Next()
		if err != nil {
			res.Err = err
			return res
		}
		now := r.now()
		attempt := ProbeAttempt{Instance: intent.Instance, AttemptID: p.AttemptID, Windows: p.Windows, SendFenceSeq: fence, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second), SuppressUntil: now.Add(10 * time.Minute)}
		if err = r.probeWAL.PersistSending(attempt); err != nil {
			res.Err = err
			return res
		}
		held.MarkProbeSent(attempt.SuppressUntil)
		err = held.DoHTTP(ctx, func(context.Context) error {
			return r.probeWAL.ExecuteSend(func() error {
				resp, e := r.host.Do(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint, Headers: http.Header{"Authorization": []string{"Bearer " + p.Credentials.AccessToken}, "Chatgpt-Account-Id": []string{p.Credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}}, Body: resetProbePayloadBytes()})
				if e != nil {
					return e
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return fmt.Errorf("probe status %d", resp.StatusCode)
				}
				return nil
			})
		})
		if err != nil {
			res.Err = err
			return res
		}
		if err = r.probeWAL.PersistSent(intent.Instance, r.now()); err != nil {
			res.Err = err
			return res
		}
		if err = held.WaitPropagation(ctx, 3*time.Second); err != nil {
			res.Err = err
			return res
		}
		res.Value = fence
	}
	return res
}

func (r *QuotaRefresher) typedFetchQuota(ctx context.Context, held *HeldLease, credentials CodexCredentials) (ParsedQuota, error) {
	var resp pluginapi.HTTPResponse
	err := held.DoHTTP(ctx, func(context.Context) error {
		var err error
		resp, err = r.host.Do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: r.state.Config().QuotaEndpoint, Headers: http.Header{
			"Authorization": []string{"Bearer " + credentials.AccessToken}, "Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}, "User-Agent": []string{quotaUserAgent},
		}})
		return err
	})
	if err != nil {
		return ParsedQuota{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, quotaStatusError{status: resp.StatusCode, body: resp.Body}
	}
	return ParseCodexUsagePayload(resp.Body, r.now())
}
func probeSnapshots(q ParsedQuota) map[ProbeWindowKind]QuotaSnapshot {
	out := map[ProbeWindowKind]QuotaSnapshot{}
	for _, p := range []struct {
		k ProbeWindowKind
		w *QuotaWindow
	}{{ProbeWindowFiveHour, q.FiveHour}, {ProbeWindowLong, q.LongWindow}} {
		if p.w == nil {
			continue
		}
		usage := 0.0
		if p.w.UsedPercent != nil {
			usage = *p.w.UsedPercent
		}
		reset := p.w.ResetAt
		out[p.k] = QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: &usage}
	}
	return out
}
