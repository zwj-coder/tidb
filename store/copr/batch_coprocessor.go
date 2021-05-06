// Copyright 2020 PingCAP, Inc.
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

package copr

import (
	"context"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/store/tikv/logutil"
	"github.com/pingcap/tidb/store/tikv/metrics"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"github.com/pingcap/tidb/store/tikv/util"
	"github.com/pingcap/tidb/util/memory"
	"go.uber.org/zap"
)

// batchCopTask comprises of multiple copTask that will send to same store.
type batchCopTask struct {
	storeAddr string
	cmdType   tikvrpc.CmdType
	ctx       *tikv.RPCContext

	regionInfos []tikv.RegionInfo
}

type batchCopResponse struct {
	pbResp *coprocessor.BatchResponse
	detail *CopRuntimeStats

	// batch Cop Response is yet to return startKey. So batchCop cannot retry partially.
	startKey kv.Key
	err      error
	respSize int64
	respTime time.Duration
}

// GetData implements the kv.ResultSubset GetData interface.
func (rs *batchCopResponse) GetData() []byte {
	return rs.pbResp.Data
}

// GetStartKey implements the kv.ResultSubset GetStartKey interface.
func (rs *batchCopResponse) GetStartKey() kv.Key {
	return rs.startKey
}

// GetExecDetails is unavailable currently, because TiFlash has not collected exec details for batch cop.
// TODO: Will fix in near future.
func (rs *batchCopResponse) GetCopRuntimeStats() *CopRuntimeStats {
	return rs.detail
}

// MemSize returns how many bytes of memory this response use
func (rs *batchCopResponse) MemSize() int64 {
	if rs.respSize != 0 {
		return rs.respSize
	}

	// ignore rs.err
	rs.respSize += int64(cap(rs.startKey))
	if rs.detail != nil {
		rs.respSize += int64(sizeofExecDetails)
	}
	if rs.pbResp != nil {
		// Using a approximate size since it's hard to get a accurate value.
		rs.respSize += int64(rs.pbResp.Size())
	}
	return rs.respSize
}

func (rs *batchCopResponse) RespTime() time.Duration {
	return rs.respTime
}

type copTaskAndRPCContext struct {
	task          *copTask
	allStoreAddrs []string
	ctx           *tikv.RPCContext
}

func balanceBatchCopTask(originalTasks []*batchCopTask) []*batchCopTask {
	storeTaskMap := make(map[uint64]*batchCopTask)
	storeCandidateTaskMap := make(map[uint64]map[string]tikv.RegionInfo)
	totalCandidateStoreNum := 0
	totalCandidateCopTaskNum := 0
	for _, task := range originalTasks {
		batchTask := &batchCopTask{
			storeAddr:   task.storeAddr,
			cmdType:     task.cmdType,
			regionInfos: []tikv.RegionInfo{task.regionInfos[0]},
		}
		storeTaskMap[task.regionInfos[0].AllStores[0]] = batchTask
	}
	for _, task := range originalTasks {
		taskStoreID := task.regionInfos[0].AllStores[0]
		for index, ri := range task.regionInfos {
			// for each cop task, figure out the valid store num
			validStoreNum := 0
			if index == 0 {
				continue
			}
			if len(ri.AllStores) <= 1 {
				validStoreNum = 1
			} else {
				for _, storeID := range ri.AllStores {
					if _, ok := storeTaskMap[storeID]; ok {
						validStoreNum++
					}
				}
			}
			if validStoreNum == 1 {
				// if only one store is valid, just put it to storeTaskMap
				storeTaskMap[taskStoreID].regionInfos = append(storeTaskMap[taskStoreID].regionInfos, ri)
			} else {
				// if more than one store is valid, put the cop task
				// to store candidate map
				totalCandidateStoreNum += validStoreNum
				totalCandidateCopTaskNum += 1
				/// put this cop task to candidate task map
				taskKey := ri.Region.String()
				for _, storeID := range ri.AllStores {
					if candidateMap, ok := storeCandidateTaskMap[storeID]; ok {
						if _, ok := candidateMap[taskKey]; ok {
							// duplicated region, should not happen, just give up balance
							return originalTasks
						}
						candidateMap[taskKey] = ri
					} else {
						candidateMap := make(map[string]tikv.RegionInfo)
						candidateMap[taskKey] = ri
						storeCandidateTaskMap[storeID] = candidateMap
					}
				}
			}
		}
	}

	avgStorePerTask := float64(totalCandidateStoreNum) / float64(totalCandidateCopTaskNum)
	findNextStore := func() (uint64, float64) {
		store := uint64(math.MaxUint64)
		possibleTaskNum := float64(0)
		for storeID := range storeTaskMap {
			if store == uint64(math.MaxUint64) && len(storeCandidateTaskMap[storeID]) > 0 {
				store = storeID
				possibleTaskNum = float64(len(storeCandidateTaskMap[storeID]))/avgStorePerTask + float64(len(storeTaskMap[storeID].regionInfos))
			} else {
				num := float64(len(storeCandidateTaskMap[storeID])) / avgStorePerTask
				if num == 0 {
					continue
				}
				num += float64(len(storeTaskMap[storeID].regionInfos))
				if num < possibleTaskNum {
					store = storeID
					possibleTaskNum = num
				}
			}
		}
		return store, possibleTaskNum
	}
	if totalCandidateStoreNum == 0 {
		return originalTasks
	}
	store, possibleTaskNum := findNextStore()
	for totalCandidateStoreNum > 0 {
		if len(storeCandidateTaskMap[store]) == 0 {
			store, possibleTaskNum = findNextStore()
		}
		for key, ri := range storeCandidateTaskMap[store] {
			storeTaskMap[store].regionInfos = append(storeTaskMap[store].regionInfos, ri)
			totalCandidateCopTaskNum--
			for _, id := range ri.AllStores {
				if _, ok := storeCandidateTaskMap[id]; ok {
					delete(storeCandidateTaskMap[id], key)
					totalCandidateStoreNum--
				}
			}
			if totalCandidateCopTaskNum > 0 {
				possibleTaskNum = float64(len(storeCandidateTaskMap[store]))/avgStorePerTask + float64(len(storeTaskMap[store].regionInfos))
				avgStorePerTask = float64(totalCandidateStoreNum) / float64(totalCandidateCopTaskNum)
				for _, id := range ri.AllStores {
					if id != store && len(storeCandidateTaskMap[id]) > 0 && float64(len(storeCandidateTaskMap[id]))/avgStorePerTask+float64(len(storeTaskMap[id].regionInfos)) <= possibleTaskNum {
						store = id
						possibleTaskNum = float64(len(storeCandidateTaskMap[id]))/avgStorePerTask + float64(len(storeTaskMap[id].regionInfos))
					}
				}
			}
			break
		}
	}

	var ret []*batchCopTask
	for _, task := range storeTaskMap {
		ret = append(ret, task)
	}
	return ret
}

func buildBatchCopTasks(bo *tikv.Backoffer, cache *tikv.RegionCache, ranges *tikv.KeyRanges, storeType kv.StoreType) ([]*batchCopTask, error) {
	start := time.Now()
	const cmdType = tikvrpc.CmdBatchCop
	rangesLen := ranges.Len()
	for {
		var tasks []*copTask
		appendTask := func(regionWithRangeInfo *tikv.KeyLocation, ranges *tikv.KeyRanges) {
			tasks = append(tasks, &copTask{
				region:    regionWithRangeInfo.Region,
				ranges:    ranges,
				cmdType:   cmdType,
				storeType: storeType,
			})
		}

		err := tikv.SplitKeyRanges(bo, cache, ranges, appendTask)
		if err != nil {
			return nil, errors.Trace(err)
		}

		var batchTasks []*batchCopTask

		storeTaskMap := make(map[string]*batchCopTask)
		needRetry := false
		for _, task := range tasks {
			rpcCtx, err := cache.GetTiFlashRPCContext(bo, task.region, false)
			if err != nil {
				return nil, errors.Trace(err)
			}
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We should retry and generate new tasks.
			if rpcCtx == nil {
				needRetry = true
				logutil.BgLogger().Info("retry for TiFlash peer with region missing", zap.Uint64("region id", task.region.GetID()))
				// Probably all the regions are invalid. Make the loop continue and mark all the regions invalid.
				// Then `splitRegion` will reloads these regions.
				continue
			}
			allStores := cache.GetAllValidTiFlashStores(task.region, rpcCtx.Store)
			if batchCop, ok := storeTaskMap[rpcCtx.Addr]; ok {
				batchCop.regionInfos = append(batchCop.regionInfos, tikv.RegionInfo{task.region, rpcCtx.Meta, task.ranges, allStores})
			} else {
				batchTask := &batchCopTask{
					storeAddr:   rpcCtx.Addr,
					cmdType:     cmdType,
					ctx:         rpcCtx,
					regionInfos: []tikv.RegionInfo{{task.region, rpcCtx.Meta, task.ranges, allStores}},
				}
				storeTaskMap[rpcCtx.Addr] = batchTask
			}
		}
		if needRetry {
			// Backoff once for each retry.
			err = bo.Backoff(tikv.BoRegionMiss, errors.New("Cannot find region with TiFlash peer"))
			if err != nil {
				return nil, errors.Trace(err)
			}
			continue
		}

		for _, task := range storeTaskMap {
			batchTasks = append(batchTasks, task)
		}
		batchTasks = balanceBatchCopTask(batchTasks)

		if elapsed := time.Since(start); elapsed > time.Millisecond*500 {
			logutil.BgLogger().Warn("buildBatchCopTasks takes too much time",
				zap.Duration("elapsed", elapsed),
				zap.Int("range len", rangesLen),
				zap.Int("task len", len(batchTasks)))
		}
		metrics.TxnRegionsNumHistogramWithBatchCoprocessor.Observe(float64(len(batchTasks)))
		return batchTasks, nil
	}
}

func (c *CopClient) sendBatch(ctx context.Context, req *kv.Request, vars *kv.Variables) kv.Response {
	if req.KeepOrder || req.Desc {
		return copErrorResponse{errors.New("batch coprocessor cannot prove keep order or desc property")}
	}
	ctx = context.WithValue(ctx, tikv.TxnStartKey, req.StartTs)
	bo := tikv.NewBackofferWithVars(ctx, copBuildTaskMaxBackoff, vars)
	tasks, err := buildBatchCopTasks(bo, c.store.GetRegionCache(), tikv.NewKeyRanges(req.KeyRanges), req.StoreType)
	if err != nil {
		return copErrorResponse{err}
	}
	it := &batchCopIterator{
		store:        c.store.KVStore,
		req:          req,
		finishCh:     make(chan struct{}),
		vars:         vars,
		memTracker:   req.MemTracker,
		ClientHelper: tikv.NewClientHelper(c.store.KVStore, util.NewTSSet(5)),
		rpcCancel:    tikv.NewRPCanceller(),
	}
	ctx = context.WithValue(ctx, tikv.RPCCancellerCtxKey{}, it.rpcCancel)
	it.tasks = tasks
	it.respChan = make(chan *batchCopResponse, 2048)
	go it.run(ctx)
	return it
}

type batchCopIterator struct {
	*tikv.ClientHelper

	store    *tikv.KVStore
	req      *kv.Request
	finishCh chan struct{}

	tasks []*batchCopTask

	// Batch results are stored in respChan.
	respChan chan *batchCopResponse

	vars *kv.Variables

	memTracker *memory.Tracker

	rpcCancel *tikv.RPCCanceller

	wg sync.WaitGroup
	// closed represents when the Close is called.
	// There are two cases we need to close the `finishCh` channel, one is when context is done, the other one is
	// when the Close is called. we use atomic.CompareAndSwap `closed` to to make sure the channel is not closed twice.
	closed uint32
}

func (b *batchCopIterator) run(ctx context.Context) {
	// We run workers for every batch cop.
	for _, task := range b.tasks {
		b.wg.Add(1)
		bo := tikv.NewBackofferWithVars(ctx, copNextMaxBackoff, b.vars)
		go b.handleTask(ctx, bo, task)
	}
	b.wg.Wait()
	close(b.respChan)
}

// Next returns next coprocessor result.
// NOTE: Use nil to indicate finish, so if the returned ResultSubset is not nil, reader should continue to call Next().
func (b *batchCopIterator) Next(ctx context.Context) (kv.ResultSubset, error) {
	var (
		resp   *batchCopResponse
		ok     bool
		closed bool
	)

	// Get next fetched resp from chan
	resp, ok, closed = b.recvFromRespCh(ctx)
	if !ok || closed {
		return nil, nil
	}

	if resp.err != nil {
		return nil, errors.Trace(resp.err)
	}

	err := b.store.CheckVisibility(b.req.StartTs)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return resp, nil
}

func (b *batchCopIterator) recvFromRespCh(ctx context.Context) (resp *batchCopResponse, ok bool, exit bool) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case resp, ok = <-b.respChan:
			return
		case <-ticker.C:
			if atomic.LoadUint32(b.vars.Killed) == 1 {
				resp = &batchCopResponse{err: tikv.ErrQueryInterrupted}
				ok = true
				return
			}
		case <-b.finishCh:
			exit = true
			return
		case <-ctx.Done():
			// We select the ctx.Done() in the thread of `Next` instead of in the worker to avoid the cost of `WithCancel`.
			if atomic.CompareAndSwapUint32(&b.closed, 0, 1) {
				close(b.finishCh)
			}
			exit = true
			return
		}
	}
}

// Close releases the resource.
func (b *batchCopIterator) Close() error {
	if atomic.CompareAndSwapUint32(&b.closed, 0, 1) {
		close(b.finishCh)
	}
	b.rpcCancel.CancelAll()
	b.wg.Wait()
	return nil
}

func (b *batchCopIterator) handleTask(ctx context.Context, bo *tikv.Backoffer, task *batchCopTask) {
	tasks := []*batchCopTask{task}
	for idx := 0; idx < len(tasks); idx++ {
		ret, err := b.handleTaskOnce(ctx, bo, tasks[idx])
		if err != nil {
			resp := &batchCopResponse{err: errors.Trace(err), detail: new(CopRuntimeStats)}
			b.sendToRespCh(resp)
			break
		}
		tasks = append(tasks, ret...)
	}
	b.wg.Done()
}

// Merge all ranges and request again.
func (b *batchCopIterator) retryBatchCopTask(ctx context.Context, bo *tikv.Backoffer, batchTask *batchCopTask) ([]*batchCopTask, error) {
	var ranges []kv.KeyRange
	for _, ri := range batchTask.regionInfos {
		ri.Ranges.Do(func(ran *kv.KeyRange) {
			ranges = append(ranges, *ran)
		})
	}
	return buildBatchCopTasks(bo, b.store.GetRegionCache(), tikv.NewKeyRanges(ranges), b.req.StoreType)
}

func (b *batchCopIterator) handleTaskOnce(ctx context.Context, bo *tikv.Backoffer, task *batchCopTask) ([]*batchCopTask, error) {
	sender := tikv.NewRegionBatchRequestSender(b.store.GetRegionCache(), b.store.GetTiKVClient())
	var regionInfos = make([]*coprocessor.RegionInfo, 0, len(task.regionInfos))
	for _, ri := range task.regionInfos {
		regionInfos = append(regionInfos, &coprocessor.RegionInfo{
			RegionId: ri.Region.GetID(),
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: ri.Region.GetConfVer(),
				Version: ri.Region.GetVer(),
			},
			Ranges: ri.Ranges.ToPBRanges(),
		})
	}

	copReq := coprocessor.BatchRequest{
		Tp:        b.req.Tp,
		StartTs:   b.req.StartTs,
		Data:      b.req.Data,
		SchemaVer: b.req.SchemaVar,
		Regions:   regionInfos,
	}

	req := tikvrpc.NewRequest(task.cmdType, &copReq, kvrpcpb.Context{
		IsolationLevel: tikv.IsolationLevelToPB(b.req.IsolationLevel),
		Priority:       tikv.PriorityToPB(b.req.Priority),
		NotFillCache:   b.req.NotFillCache,
		RecordTimeStat: true,
		RecordScanStat: true,
		TaskId:         b.req.TaskID,
	})
	req.StoreTp = kv.TiFlash

	logutil.BgLogger().Debug("send batch request to ", zap.String("req info", req.String()), zap.Int("cop task len", len(task.regionInfos)))
	resp, retry, cancel, err := sender.SendReqToAddr(bo, task.ctx, task.regionInfos, req, tikv.ReadTimeoutUltraLong)
	// If there are store errors, we should retry for all regions.
	if retry {
		return b.retryBatchCopTask(ctx, bo, task)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer cancel()
	return nil, b.handleStreamedBatchCopResponse(ctx, bo, resp.Resp.(*tikvrpc.BatchCopStreamResponse), task)
}

func (b *batchCopIterator) handleStreamedBatchCopResponse(ctx context.Context, bo *tikv.Backoffer, response *tikvrpc.BatchCopStreamResponse, task *batchCopTask) (err error) {
	defer response.Close()
	resp := response.BatchResponse
	if resp == nil {
		// streaming request returns io.EOF, so the first Response is nil.
		return
	}
	for {
		err = b.handleBatchCopResponse(bo, resp, task)
		if err != nil {
			return errors.Trace(err)
		}
		resp, err = response.Recv()
		if err != nil {
			if errors.Cause(err) == io.EOF {
				return nil
			}

			if err1 := bo.Backoff(tikv.BoTiKVRPC, errors.Errorf("recv stream response error: %v, task store addr: %s", err, task.storeAddr)); err1 != nil {
				return errors.Trace(err)
			}

			// No coprocessor.Response for network error, rebuild task based on the last success one.
			if errors.Cause(err) == context.Canceled {
				logutil.BgLogger().Info("stream recv timeout", zap.Error(err))
			} else {
				logutil.BgLogger().Info("stream unknown error", zap.Error(err))
			}
			return tikv.ErrTiFlashServerTimeout
		}
	}
}

func (b *batchCopIterator) handleBatchCopResponse(bo *tikv.Backoffer, response *coprocessor.BatchResponse, task *batchCopTask) (err error) {
	if otherErr := response.GetOtherError(); otherErr != "" {
		err = errors.Errorf("other error: %s", otherErr)
		logutil.BgLogger().Warn("other error",
			zap.Uint64("txnStartTS", b.req.StartTs),
			zap.String("storeAddr", task.storeAddr),
			zap.Error(err))
		return errors.Trace(err)
	}

	resp := batchCopResponse{
		pbResp: response,
		detail: new(CopRuntimeStats),
	}

	backoffTimes := bo.GetBackoffTimes()
	resp.detail.BackoffTime = time.Duration(bo.GetTotalSleep()) * time.Millisecond
	resp.detail.BackoffSleep = make(map[string]time.Duration, len(backoffTimes))
	resp.detail.BackoffTimes = make(map[string]int, len(backoffTimes))
	for backoff := range backoffTimes {
		backoffName := backoff.String()
		resp.detail.BackoffTimes[backoffName] = backoffTimes[backoff]
		resp.detail.BackoffSleep[backoffName] = time.Duration(bo.GetBackoffSleepMS()[backoff]) * time.Millisecond
	}
	resp.detail.CalleeAddress = task.storeAddr

	b.sendToRespCh(&resp)

	return
}

func (b *batchCopIterator) sendToRespCh(resp *batchCopResponse) (exit bool) {
	select {
	case b.respChan <- resp:
	case <-b.finishCh:
		exit = true
	}
	return
}
