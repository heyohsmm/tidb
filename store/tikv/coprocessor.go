// Copyright 2016 PingCAP, Inc.
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
	"bytes"
	goctx "context"
	"fmt"
	"io"
	"io/ioutil"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tipb/go-tipb"
)

// CopClient is coprocessor client.
type CopClient struct {
	store *tikvStore
}

// SupportRequestType checks whether reqType is supported.
func (c *CopClient) SupportRequestType(reqType, subType int64) bool {
	switch reqType {
	case kv.ReqTypeSelect, kv.ReqTypeIndex:
		switch subType {
		case kv.ReqSubTypeGroupBy, kv.ReqSubTypeBasic, kv.ReqSubTypeTopN:
			return true
		default:
			return supportExpr(tipb.ExprType(subType))
		}
	}
	return false
}

func supportExpr(exprType tipb.ExprType) bool {
	switch exprType {
	case tipb.ExprType_Null, tipb.ExprType_Int64, tipb.ExprType_Uint64, tipb.ExprType_String, tipb.ExprType_Bytes,
		tipb.ExprType_MysqlDuration, tipb.ExprType_MysqlTime, tipb.ExprType_MysqlDecimal,
		tipb.ExprType_ColumnRef,
		tipb.ExprType_And, tipb.ExprType_Or,
		tipb.ExprType_LT, tipb.ExprType_LE, tipb.ExprType_EQ, tipb.ExprType_NE,
		tipb.ExprType_GE, tipb.ExprType_GT, tipb.ExprType_NullEQ,
		tipb.ExprType_In, tipb.ExprType_ValueList,
		tipb.ExprType_Like, tipb.ExprType_Not:
		return true
	case tipb.ExprType_Plus, tipb.ExprType_Div:
		return true
	case tipb.ExprType_Case, tipb.ExprType_If:
		return true
	case tipb.ExprType_Count, tipb.ExprType_First, tipb.ExprType_Max, tipb.ExprType_Min, tipb.ExprType_Sum, tipb.ExprType_Avg:
		return true
	case kv.ReqSubTypeDesc:
		return true
	default:
		return false
	}
}

// Send builds the request and gets the coprocessor iterator response.
func (c *CopClient) Send(ctx goctx.Context, req *kv.Request) kv.Response {
	coprocessorCounter.WithLabelValues("send").Inc()

	bo := NewBackoffer(copBuildTaskMaxBackoff, ctx)
	tasks, err := buildCopTasks(bo, c.store.regionCache, &copRanges{mid: req.KeyRanges}, req.Desc)
	if err != nil {
		return copErrorResponse{err}
	}
	it := &copIterator{
		store:       c.store,
		req:         req,
		concurrency: req.Concurrency,
		finished:    make(chan struct{}),
	}
	it.tasks = tasks
	if it.concurrency > len(tasks) {
		it.concurrency = len(tasks)
	}
	if it.concurrency < 1 {
		// Make sure that there is at least one worker.
		it.concurrency = 1
	}
	if !it.req.KeepOrder {
		it.respChan = make(chan *coprocessor.Response, it.concurrency)
	}
	it.taskCh = make(chan *copTask, req.Concurrency)
	it.errChan = make(chan error, it.concurrency)
	it.run(ctx)
	return it
}

// copTask contains a related Region and KeyRange for a kv.Request.
type copTask struct {
	region RegionVerID
	ranges *copRanges

	respChan  chan *coprocessor.Response
	storeAddr string
}

func (r *copTask) String() string {
	return fmt.Sprintf("region(%d %d %d) ranges(%d) store(%s)",
		r.region.id, r.region.confVer, r.region.ver, r.ranges.len(), r.storeAddr)
}

// copRanges is like []kv.KeyRange, but may has extra elements at head/tail.
// It's for avoiding alloc big slice during build copTask.
type copRanges struct {
	first *kv.KeyRange
	mid   []kv.KeyRange
	last  *kv.KeyRange
}

func (r *copRanges) String() string {
	var s string
	r.do(func(ran *kv.KeyRange) {
		s += fmt.Sprintf("[%q, %q]", ran.StartKey, ran.EndKey)
	})
	return s
}

func (r *copRanges) len() int {
	var l int
	if r.first != nil {
		l++
	}
	l += len(r.mid)
	if r.last != nil {
		l++
	}
	return l
}

func (r *copRanges) at(i int) kv.KeyRange {
	if r.first != nil {
		if i == 0 {
			return *r.first
		}
		i--
	}
	if i < len(r.mid) {
		return r.mid[i]
	}
	return *r.last
}

func (r *copRanges) slice(from, to int) *copRanges {
	var ran copRanges
	if r.first != nil {
		if from == 0 && to > 0 {
			ran.first = r.first
		}
		if from > 0 {
			from--
		}
		if to > 0 {
			to--
		}
	}
	if to <= len(r.mid) {
		ran.mid = r.mid[from:to]
	} else {
		if from <= len(r.mid) {
			ran.mid = r.mid[from:]
		}
		if from < to {
			ran.last = r.last
		}
	}
	return &ran
}

func (r *copRanges) do(f func(ran *kv.KeyRange)) {
	if r.first != nil {
		f(r.first)
	}
	for _, ran := range r.mid {
		f(&ran)
	}
	if r.last != nil {
		f(r.last)
	}
}

func (r *copRanges) toPBRanges() []*coprocessor.KeyRange {
	ranges := make([]*coprocessor.KeyRange, 0, r.len())
	r.do(func(ran *kv.KeyRange) {
		ranges = append(ranges, &coprocessor.KeyRange{
			Start: ran.StartKey,
			End:   ran.EndKey,
		})
	})
	return ranges
}

func buildCopTasks(bo *Backoffer, cache *RegionCache, ranges *copRanges, desc bool) ([]*copTask, error) {
	coprocessorCounter.WithLabelValues("build_task").Inc()

	start := time.Now()
	rangesLen := ranges.len()

	var tasks []*copTask
	appendTask := func(region RegionVerID, ranges *copRanges) {
		newTask := &copTask{
			region:   region,
			ranges:   ranges,
			respChan: make(chan *coprocessor.Response, 1),
		}
		tasks = append(tasks, newTask)
	}

	for ranges.len() > 0 {
		loc, err := cache.LocateKey(bo, ranges.at(0).StartKey)
		if err != nil {
			return nil, errors.Trace(err)
		}

		// Iterate to the first range that is not complete in the region.
		var i int
		for ; i < ranges.len(); i++ {
			r := ranges.at(i)
			if !(loc.Contains(r.EndKey) || bytes.Equal(loc.EndKey, r.EndKey)) {
				break
			}
		}
		// All rest ranges belong to the same region.
		if i == ranges.len() {
			appendTask(loc.Region, ranges)
			break
		}

		r := ranges.at(i)
		if loc.Contains(r.StartKey) {
			// Part of r is not in the region. We need to split it.
			taskRanges := ranges.slice(0, i)
			taskRanges.last = &kv.KeyRange{
				StartKey: r.StartKey,
				EndKey:   loc.EndKey,
			}
			appendTask(loc.Region, taskRanges)

			ranges = ranges.slice(i+1, ranges.len())
			ranges.first = &kv.KeyRange{
				StartKey: loc.EndKey,
				EndKey:   r.EndKey,
			}
		} else {
			// rs[i] is not in the region.
			appendTask(loc.Region, ranges.slice(0, i))
			ranges = ranges.slice(i, ranges.len())
		}
	}

	if desc {
		reverseTasks(tasks)
	}
	if elapsed := time.Since(start); elapsed > time.Millisecond*500 {
		log.Warnf("buildCopTasks takes too much time (%v), range len %v, task len %v", elapsed, rangesLen, len(tasks))
	}
	txnRegionsNumHistogram.WithLabelValues("coprocessor").Observe(float64(len(tasks)))
	return tasks, nil
}

func reverseTasks(tasks []*copTask) {
	for i := 0; i < len(tasks)/2; i++ {
		j := len(tasks) - i - 1
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
}

type copIterator struct {
	store       *tikvStore
	req         *kv.Request
	concurrency int
	finished    chan struct{}
	taskCh      chan *copTask

	// If keepOrder, result is stored in copTask.respChan, read them out one by one.
	tasks []*copTask
	curr  int

	// Otherwise, result is stored in respChan.
	respChan chan *coprocessor.Response
	errChan  chan error
	pending  int64
}

const minLogCopTaskTime = 300 * time.Millisecond

// The worker function that get a copTask from channel, handle it and
// send the result back.
func (it *copIterator) work(ctx goctx.Context, taskCh <-chan *copTask) {
	for task := range taskCh {
		bo := NewBackoffer(copNextMaxBackoff, ctx)
		startTime := time.Now()
		resps, err := it.handleTask(bo, task)
		costTime := time.Since(startTime)
		if costTime > minLogCopTaskTime {
			log.Infof("[TIME_COP_TASK] %s%s %s", costTime, bo, task)
		}
		coprocessorHistogram.Observe(costTime.Seconds())
		if bo.totalSleep > 0 {
			backoffHistogram.Observe(float64(bo.totalSleep) / 1000)
		}
		if err != nil {
			it.errChan <- err
			break
		}
		var ch chan *coprocessor.Response
		if !it.req.KeepOrder {
			ch = it.respChan
		} else {
			ch = task.respChan
		}

		for _, resp := range resps {
			select {
			case ch <- resp:
			case <-ctx.Done():
				return
			case <-it.finished:
				return
			}
		}
	}
}

func (it *copIterator) run(ctx goctx.Context) {
	// Start it.concurrency number of workers to handle cop requests.
	for i := 0; i < it.concurrency; i++ {
		go it.work(ctx, it.taskCh)
	}

	atomic.AddInt64(&it.pending, int64(len(it.tasks)))
	go func() {
		for _, t := range it.tasks {
			it.taskCh <- t
		}
	}()
}

// Return next coprocessor result.
func (it *copIterator) Next() (io.ReadCloser, error) {
	coprocessorCounter.WithLabelValues("next").Inc()

	var (
		resp *coprocessor.Response
		err  error
	)
	// If data order matters, response should be returned in the same order as copTask list.
	// Otherwise all responses are returned from a single channel.
	if !it.req.KeepOrder {
		if atomic.LoadInt64(&it.pending) == 0 {
			close(it.taskCh)
			return nil, nil
		}

		// Get next fetched resp from chan
		select {
		case resp = <-it.respChan:
		case err = <-it.errChan:
		}
		atomic.AddInt64(&it.pending, -1)
	} else {
		if it.curr == len(it.tasks) {
			// resp will be nil if iterator is finished.
			close(it.taskCh)
			return nil, nil
		}
		task := it.tasks[it.curr]

		select {
		case resp = <-task.respChan:
		case err = <-it.errChan:
		}
		it.curr++
	}

	if err != nil {
		return nil, errors.Trace(err)
	}
	return ioutil.NopCloser(bytes.NewBuffer(resp.Data)), nil
}

// Handle single copTask.
func (it *copIterator) handleTask(bo *Backoffer, task *copTask) ([]*coprocessor.Response, error) {
	coprocessorCounter.WithLabelValues("handle_task").Inc()
	sender := NewRegionRequestSender(bo, it.store.regionCache, it.store.client)
	for {
		select {
		case <-it.finished:
			return nil, nil
		default:
		}

		req := &coprocessor.Request{
			Tp:     it.req.Tp,
			Data:   it.req.Data,
			Ranges: task.ranges.toPBRanges(),
		}
		resp, err := sender.SendCopReq(req, task.region, readTimeoutMedium)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if regionErr := resp.GetRegionError(); regionErr != nil {
			err = bo.Backoff(boRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return nil, errors.Trace(err)
			}
			ret, err := it.rebuildCurrentTask(bo, task)
			if err != nil {
				return nil, errors.Trace(err)
			}
			return ret, nil
		}
		if e := resp.GetLocked(); e != nil {
			log.Debugf("coprocessor encounters lock: %v", e)
			ok, err1 := it.store.lockResolver.ResolveLocks(bo, []*Lock{newLock(e)})
			if err1 != nil {
				return nil, errors.Trace(err1)
			}
			if !ok {
				err = bo.Backoff(boTxnLockFast, errors.New(e.String()))
				if err != nil {
					return nil, errors.Trace(err)
				}
			}
			continue
		}
		if e := resp.GetOtherError(); e != "" {
			err = errors.Errorf("other error: %s", e)
			log.Warnf("coprocessor err: %v", err)
			return nil, errors.Trace(err)
		}
		task.storeAddr = sender.storeAddr
		return []*coprocessor.Response{resp}, nil
	}
}

// Rebuild and handle current task. It may be split into multiple tasks (in region split scenario).
func (it *copIterator) rebuildCurrentTask(bo *Backoffer, task *copTask) ([]*coprocessor.Response, error) {
	coprocessorCounter.WithLabelValues("rebuild_task").Inc()

	newTasks, err := buildCopTasks(bo, it.store.regionCache, task.ranges, it.req.Desc)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if newTasks == nil {
		// TODO: check this, this should never happen.
		return nil, nil
	}

	var ret []*coprocessor.Response
	for _, t := range newTasks {
		resp, err := it.handleTask(bo, t)
		if err != nil {
			return nil, errors.Trace(err)
		}
		ret = append(ret, resp...)
	}
	return ret, nil
}

func (it *copIterator) Close() error {
	close(it.finished)
	return nil
}

// copErrorResponse returns error when calling Next()
type copErrorResponse struct{ error }

func (it copErrorResponse) Next() (io.ReadCloser, error) {
	return nil, it.error
}

func (it copErrorResponse) Close() error {
	return nil
}
