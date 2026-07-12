package main

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// LegacyRefreshTxn is the S3 migration envelope.  It owns one coordinator
// instance lease for the complete pre-S4 refresh sequence.  The wrapped host
// makes every external operation consume an HTTP slot without submitting a
// nested intent.
type LegacyRefreshTxn struct{ refresher *QuotaRefresher }

func (t *LegacyRefreshTxn) RunHeld(ctx context.Context, intent Intent, held ...*HeldLease) OperationResult {
	result := OperationResult{Token: intent.Token, Generation: intent.Generation}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	auth, ok := intent.Payload.(pluginapi.HostAuthFileEntry)
	if !ok {
		result.Err = errors.New("legacy refresh payload is not a host auth entry")
		return result
	}
	if t == nil || t.refresher == nil {
		result.Err = errors.New("legacy refresh transaction has no refresher")
		return result
	}
	r := QuotaRefresher{host: t.refresher.host, state: t.refresher.state, now: t.refresher.now}
	if len(held) > 0 && held[0] != nil {
		r.host = heldHostClient{HostClient: t.refresher.host, ctx: ctx, lease: held[0]}
	}
	r.refreshAuthVersionedHeld(auth, uint64(intent.Generation))
	if err := ctx.Err(); err != nil {
		result.Err = err
	}
	return result
}

type heldHostClient struct {
	HostClient
	ctx   context.Context
	lease *HeldLease
}

func (h heldHostClient) GetAuth(authIndex string) (out pluginapi.HostAuthGetResponse, err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error { out, err = h.HostClient.GetAuth(authIndex); return err })
	return out, err
}
func (h heldHostClient) SaveAuth(name string, raw json.RawMessage) (err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error { err = h.HostClient.SaveAuth(name, raw); return err })
	return err
}
func (h heldHostClient) Do(req pluginapi.HTTPRequest) (out pluginapi.HTTPResponse, err error) {
	if req.URL == codexResetProbeEndpoint {
		h.lease.MarkProbeSent(h.lease.coordinator.opts.Now().Add(10 * time.Minute))
	}
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error { out, err = h.HostClient.Do(req); return err })
	return out, err
}

func legacyAuthInstanceID(authID string) AuthInstanceID {
	h := fnv.New64a()
	_, _ = h.Write([]byte(authID))
	v := h.Sum64()
	if v == 0 {
		v = 1
	}
	return AuthInstanceID(v)
}
