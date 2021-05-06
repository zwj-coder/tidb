// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RegionInfo struct {
	Region    RegionVerID
	Meta      *metapb.Region
	Ranges    *KeyRanges
	AllStores []uint64
}

// RegionBatchRequestSender sends BatchCop requests to TiFlash server by stream way.
type RegionBatchRequestSender struct {
	*RegionRequestSender
}

// NewRegionBatchRequestSender creates a RegionBatchRequestSender object.
func NewRegionBatchRequestSender(cache *RegionCache, client Client) *RegionBatchRequestSender {
	return &RegionBatchRequestSender{
		RegionRequestSender: NewRegionRequestSender(cache, client),
	}
}

// SendReqToAddr sends a request to tikv/tiflash server.
func (ss *RegionBatchRequestSender) SendReqToAddr(bo *Backoffer, rpcCtx *RPCContext, regionInfos []RegionInfo, req *tikvrpc.Request, timout time.Duration) (resp *tikvrpc.Response, retry bool, cancel func(), err error) {
	// use the first ctx to send request, because every ctx has same address.
	cancel = func() {}
	if e := tikvrpc.SetContext(req, rpcCtx.Meta, rpcCtx.Peer); e != nil {
		return nil, false, cancel, errors.Trace(e)
	}
	ctx := bo.GetCtx()
	if rawHook := ctx.Value(RPCCancellerCtxKey{}); rawHook != nil {
		ctx, cancel = rawHook.(*RPCCanceller).WithCancel(ctx)
	}
	start := time.Now()
	resp, err = ss.GetClient().SendRequest(ctx, rpcCtx.Addr, req, timout)
	if ss.Stats != nil {
		RecordRegionRequestRuntimeStats(ss.Stats, req.Type, time.Since(start))
	}
	if err != nil {
		cancel()
		ss.SetRPCError(err)
		e := ss.onSendFailForBatchRegions(bo, rpcCtx, regionInfos, err)
		if e != nil {
			return nil, false, func() {}, errors.Trace(e)
		}
		return nil, true, func() {}, nil
	}
	// We don't need to process region error or lock error. Because TiFlash will retry by itself.
	return
}

func (ss *RegionBatchRequestSender) onSendFailForBatchRegions(bo *Backoffer, ctx *RPCContext, regionInfos []RegionInfo, err error) error {
	// If it failed because the context is cancelled by ourself, don't retry.
	if errors.Cause(err) == context.Canceled || status.Code(errors.Cause(err)) == codes.Canceled {
		return errors.Trace(err)
	} else if atomic.LoadUint32(&ShuttingDown) > 0 {
		return ErrTiDBShuttingDown
	}

	ss.GetRegionCache().OnSendFailForBatchRegions(bo, ctx.Store, regionInfos, ss.NeedReloadRegion(ctx), err)

	err = bo.Backoff(BoTiFlashRPC, errors.Errorf("send tikv request error: %v, ctx: %v, regionInfos: %v, try next peer later", err, ctx, regionInfos))
	return errors.Trace(err)
}
