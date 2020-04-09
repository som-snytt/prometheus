// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"container/heap"
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
)

type fanout struct {
	logger log.Logger

	primary     Storage
	secondaries []Storage
}

// NewFanout returns a new fan-out Storage, which proxies reads and writes
// through to multiple underlying storages.
func NewFanout(logger log.Logger, primary Storage, secondaries ...Storage) Storage {
	return &fanout{
		logger:      logger,
		primary:     primary,
		secondaries: secondaries,
	}
}

// StartTime implements the Storage interface.
func (f *fanout) StartTime() (int64, error) {
	// StartTime of a fanout should be the earliest StartTime of all its storages,
	// both primary and secondaries.
	firstTime, err := f.primary.StartTime()
	if err != nil {
		return int64(model.Latest), err
	}

	for _, storage := range f.secondaries {
		t, err := storage.StartTime()
		if err != nil {
			return int64(model.Latest), err
		}
		if t < firstTime {
			firstTime = t
		}
	}
	return firstTime, nil
}

func (f *fanout) Querier(ctx context.Context, mint, maxt int64) (Querier, error) {
	primary, err := f.primary.Querier(ctx, mint, maxt)
	if err != nil {
		return nil, err
	}

	secondaries := make([]Querier, 0, len(f.secondaries))
	for _, storage := range f.secondaries {
		querier, err := storage.Querier(ctx, mint, maxt)
		if err != nil {
			for _, q := range secondaries {
				// TODO(bwplotka): Log error.
				_ = q.Close()
			}
			return nil, err
		}
		secondaries = append(secondaries, querier)
	}

	return NewMergeQuerier(primary, secondaries, ChainingSeriesMerge), nil
}

func (f *fanout) ChunkQuerier(ctx context.Context, mint, maxt int64) (ChunkQuerier, error) {
	primary, err := f.primary.ChunkQuerier(ctx, mint, maxt)
	if err != nil {
		return nil, err
	}

	secondaries := make([]ChunkQuerier, 0, len(f.secondaries))
	for _, storage := range f.secondaries {
		querier, err := storage.ChunkQuerier(ctx, mint, maxt)
		if err != nil {
			for _, q := range secondaries {
				// TODO(bwplotka): Log error.
				_ = q.Close()
			}
			return nil, err
		}
		secondaries = append(secondaries, querier)
	}

	return NewMergeChunkQuerier(primary, secondaries, NewCompactingChunkSeriesMerger(ChainingSeriesMerge)), nil
}

func (f *fanout) Appender() Appender {
	primary := f.primary.Appender()
	secondaries := make([]Appender, 0, len(f.secondaries))
	for _, storage := range f.secondaries {
		secondaries = append(secondaries, storage.Appender())
	}
	return &fanoutAppender{
		logger:      f.logger,
		primary:     primary,
		secondaries: secondaries,
	}
}

// Close closes the storage and all its underlying resources.
func (f *fanout) Close() error {
	if err := f.primary.Close(); err != nil {
		return err
	}

	// TODO return multiple errors?
	var lastErr error
	for _, storage := range f.secondaries {
		if err := storage.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// fanoutAppender implements Appender.
type fanoutAppender struct {
	logger log.Logger

	primary     Appender
	secondaries []Appender
}

func (f *fanoutAppender) Add(l labels.Labels, t int64, v float64) (uint64, error) {
	ref, err := f.primary.Add(l, t, v)
	if err != nil {
		return ref, err
	}

	for _, appender := range f.secondaries {
		if _, err := appender.Add(l, t, v); err != nil {
			return 0, err
		}
	}
	return ref, nil
}

func (f *fanoutAppender) AddFast(ref uint64, t int64, v float64) error {
	if err := f.primary.AddFast(ref, t, v); err != nil {
		return err
	}

	for _, appender := range f.secondaries {
		if err := appender.AddFast(ref, t, v); err != nil {
			return err
		}
	}
	return nil
}

func (f *fanoutAppender) Commit() (err error) {
	err = f.primary.Commit()

	for _, appender := range f.secondaries {
		if err == nil {
			err = appender.Commit()
		} else {
			if rollbackErr := appender.Rollback(); rollbackErr != nil {
				level.Error(f.logger).Log("msg", "Squashed rollback error on commit", "err", rollbackErr)
			}
		}
	}
	return
}

func (f *fanoutAppender) Rollback() (err error) {
	err = f.primary.Rollback()

	for _, appender := range f.secondaries {
		rollbackErr := appender.Rollback()
		if err == nil {
			err = rollbackErr
		} else if rollbackErr != nil {
			level.Error(f.logger).Log("msg", "Squashed rollback error on rollback", "err", rollbackErr)
		}
	}
	return nil
}

type mergeGenericQuerier struct {
	merge genericSeriesMergeFunc

	primary        genericQuerier
	secondaries    []genericQuerier
	failedQueriers map[genericQuerier]struct{}
	setQuerierMap  map[genericSeriesSet]genericQuerier
}

// NewMergeQuerier returns a new Querier that merges results of given primary and slice of secondary queriers.
// The difference between primary and rest is as follows:
// * If the primary querier returns an error, the query fails.
// * For secondaries it just returns warnings.
// In case of overlaps between the data given by all secondaries, merge will be used.
func NewMergeQuerier(primary Querier, secondaries []Querier, merge VerticalSeriesMergeFunc) Querier {
	filtered := make([]genericQuerier, 0, len(secondaries))
	for _, querier := range secondaries {
		if _, ok := querier.(noopQuerier); !ok && querier != nil {
			filtered = append(filtered, newGenericQuerierFrom(querier))
			continue
		}
	}

	if len(filtered) == 0 {
		return primary
	}

	return &querierAdapter{&mergeGenericQuerier{
		merge:          (&seriesMergerAdapter{VerticalSeriesMergeFunc: merge}).Merge,
		primary:        newGenericQuerierFrom(primary),
		secondaries:    filtered,
		failedQueriers: make(map[genericQuerier]struct{}),
		setQuerierMap:  make(map[genericSeriesSet]genericQuerier),
	}}
}

// NewMergeChunkQuerier returns a new ChunkQuerier that merges results of given primary and slice of secondary chunk queriers.
// The difference between primary and rest is as follows:
// * If the primary querier returns an error, the query fails.
// * For secondaries it just returns warnings.
// In case of overlaps between the data given by all secondaries, merge will be used.
// TODO(bwplotka): Currently merge will compact overlapping chunks with bigger chunk, without limit. Split it: https://github.com/prometheus/tsdb/issues/670
func NewMergeChunkQuerier(primary ChunkQuerier, secondaries []ChunkQuerier, merge VerticalChunkSeriesMergeFunc) ChunkQuerier {
	filtered := make([]genericQuerier, 0, len(secondaries))
	for _, querier := range secondaries {
		if _, ok := querier.(noopChunkQuerier); !ok && querier != nil {
			filtered = append(filtered, newGenericQuerierFromChunk(querier))
		}
	}

	if len(filtered) == 0 {
		return primary
	}

	return &chunkQuerierAdapter{&mergeGenericQuerier{
		merge:          (&chunkSeriesMergerAdapter{VerticalChunkSeriesMergeFunc: merge}).Merge,
		primary:        newGenericQuerierFromChunk(primary),
		secondaries:    filtered,
		failedQueriers: make(map[genericQuerier]struct{}),
		setQuerierMap:  make(map[genericSeriesSet]genericQuerier),
	}}
}

// Select returns a set of series that matches the given label matchers.
// NOTE: mergeGenericQuerier selects always return series sorted no matter if sorting was not required from caller.
func (q *mergeGenericQuerier) Select(_ bool, hints *SelectHints, matchers ...*labels.Matcher) (genericSeriesSet, Warnings, error) {
	var (
		seriesSets = make([]genericSeriesSet, 0, len(q.secondaries))
		warnings   Warnings
		priErr     error
		wg         sync.WaitGroup
	)

	type queryResult struct {
		q   genericQuerier
		set genericSeriesSet
		wrn Warnings
		err error
	}
	resultChan := make(chan *queryResult)

	wg.Add(1)
	go func() {
		// We need to sort for newGenericMergeSeriesSet to work.
		set, wrn, err := q.primary.Select(true, hints, matchers...)
		resultChan <- &queryResult{q: q, set: set, wrn: wrn, err: err}
		wg.Done()
	}()
	for _, q := range q.secondaries {
		wg.Add(1)
		go func(q genericQuerier) {
			// We need to sort for newGenericMergeSeriesSet to work.
			set, wrn, err := q.Select(true, hints, matchers...)
			resultChan <- &queryResult{q: q, set: set, wrn: wrn, err: err}
			wg.Done()
		}(q)
	}
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	for r := range resultChan {
		q.setQuerierMap[r.set] = r.q
		if r.wrn != nil {
			warnings = append(warnings, r.wrn...)
		}
		if r.err != nil {
			q.failedQueriers[r.q] = struct{}{}
			if r.q != q.primary {
				// If the error source is not the primary querier, return the error as a warning.
				warnings = append(warnings, errors.Wrap(r.err, "secondary querier's Select"))
				continue
			}
			// TODO(bwplotka): We could just quit, but instead we need to wait for all responses.
			// Propagate context to Select at some point to be able to do so.
			priErr = errors.Wrap(r.err, "primary querier's Select")
			continue
		}
		seriesSets = append(seriesSets, r.set)
	}
	if priErr != nil {
		return nil, warnings, priErr
	}
	return newGenericMergeSeriesSet(seriesSets, q, q.merge), warnings, nil
}

// LabelValues returns all potential values for a label name.
func (q *mergeGenericQuerier) LabelValues(name string) ([]string, Warnings, error) {
	var results [][]string
	var warnings Warnings
	for _, querier := range q.secondaries {
		values, wrn, err := querier.LabelValues(name)
		if wrn != nil {
			warnings = append(warnings, wrn...)
		}
		if err != nil {
			q.failedQueriers[querier] = struct{}{}
			if querier != q.primary {
				// If the error source isn't the primary querier, return the error as a warning.
				warnings = append(warnings, errors.Wrap(err, "secondary querier's LabelValues"))
				continue
			}
			return nil, nil, errors.Wrap(err, "primary querier's LabelValues")
		}
		results = append(results, values)
	}
	return mergeStringSlices(results), warnings, nil
}

func (q *mergeGenericQuerier) IsFailedSet(set genericSeriesSet) bool {
	_, isFailedQuerier := q.failedQueriers[q.setQuerierMap[set]]
	return isFailedQuerier
}

func mergeStringSlices(ss [][]string) []string {
	switch len(ss) {
	case 0:
		return nil
	case 1:
		return ss[0]
	case 2:
		return mergeTwoStringSlices(ss[0], ss[1])
	default:
		halfway := len(ss) / 2
		return mergeTwoStringSlices(
			mergeStringSlices(ss[:halfway]),
			mergeStringSlices(ss[halfway:]),
		)
	}
}

func mergeTwoStringSlices(a, b []string) []string {
	i, j := 0, 0
	result := make([]string, 0, len(a)+len(b))
	for i < len(a) && j < len(b) {
		switch strings.Compare(a[i], b[j]) {
		case 0:
			result = append(result, a[i])
			i++
			j++
		case -1:
			result = append(result, a[i])
			i++
		case 1:
			result = append(result, b[j])
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// LabelNames returns all the unique label names present in the block in sorted order.
func (q *mergeGenericQuerier) LabelNames() ([]string, Warnings, error) {
	labelNamesMap := make(map[string]struct{})
	var warnings Warnings
	for _, querier := range q.secondaries {
		names, wrn, err := querier.LabelNames()
		if wrn != nil {
			warnings = append(warnings, wrn...)
		}
		if err != nil {
			q.failedQueriers[querier] = struct{}{}
			if querier != q.primary {
				// If the error source isn't the primary querier, return the error as a warning.
				warnings = append(warnings, errors.Wrap(err, "secondary querier's LabelNames"))
				continue
			}
			return nil, nil, errors.Wrap(err, "primary querier's LabelNames")
		}

		for _, name := range names {
			labelNamesMap[name] = struct{}{}
		}
	}

	labelNames := make([]string, 0, len(labelNamesMap))
	for name := range labelNamesMap {
		labelNames = append(labelNames, name)
	}
	sort.Strings(labelNames)

	return labelNames, warnings, nil
}

// Close releases the resources of the Querier.
func (q *mergeGenericQuerier) Close() error {
	var errs tsdb_errors.MultiError
	for _, querier := range q.secondaries {
		if err := querier.Close(); err != nil {
			errs.Add(err)
		}
	}
	return errs.Err()
}

// genericMergeSeriesSet implements genericSeriesSet
type genericMergeSeriesSet struct {
	currentLabels labels.Labels
	mergeFunc     genericSeriesMergeFunc

	heap genericSeriesSetHeap
	sets []genericSeriesSet

	currentSets []genericSeriesSet
	querier     *mergeGenericQuerier
}

// VerticalSeriesMergeFunc returns merged series implementation that merges potentially time-overlapping series with same labels together.
type VerticalSeriesMergeFunc func(...Series) Series

// NewMergeSeriesSet returns a new SeriesSet that merges many SeriesSet together.
func NewMergeSeriesSet(sets []SeriesSet, mergeFunc VerticalSeriesMergeFunc) SeriesSet {
	genericSets := make([]genericSeriesSet, 0, len(sets))
	for _, s := range sets {
		genericSets = append(genericSets, &genericSeriesSetAdapter{s})

	}
	return &seriesSetAdapter{newGenericMergeSeriesSet(genericSets, nil, (&seriesMergerAdapter{VerticalSeriesMergeFunc: mergeFunc}).Merge)}
}

// VerticalChunkSeriesMergeFunc returns merged chunk series implementation that merges potentially time-overlapping chunk series with same labels together.
type VerticalChunkSeriesMergeFunc func(...ChunkSeries) ChunkSeries

// NewMergeChunkSeriesSet returns a new ChunkSeriesSet that merges many SeriesSet together.
func NewMergeChunkSeriesSet(sets []ChunkSeriesSet, mergeFunc VerticalChunkSeriesMergeFunc) ChunkSeriesSet {
	genericSets := make([]genericSeriesSet, 0, len(sets))
	for _, s := range sets {
		genericSets = append(genericSets, &genericChunkSeriesSetAdapter{s})

	}
	return &chunkSeriesSetAdapter{newGenericMergeSeriesSet(genericSets, nil, (&chunkSeriesMergerAdapter{VerticalChunkSeriesMergeFunc: mergeFunc}).Merge)}
}

// newGenericMergeSeriesSet returns a new genericSeriesSet that merges (and deduplicates)
// series returned by the chkQuerierSeries series sets when iterating.
// Each chkQuerierSeries series set must return its series in labels order, otherwise
// merged series set will be incorrect.
// Argument 'querier' is optional and can be nil. Pass Querier if you want to retry query in case of failing series set.
// Overlapped situations are merged using provided merge.
func newGenericMergeSeriesSet(sets []genericSeriesSet, querier *mergeGenericQuerier, mergeFunc genericSeriesMergeFunc) genericSeriesSet {
	if len(sets) == 1 {
		return sets[0]
	}

	// Sets need to be pre-advanced, so we can introspect the label of the
	// series under the cursor.
	var h genericSeriesSetHeap
	for _, set := range sets {
		if set == nil {
			continue
		}
		if set.Next() {
			heap.Push(&h, set)
		}
	}
	return &genericMergeSeriesSet{
		mergeFunc: mergeFunc,
		heap:      h,
		sets:      sets,
		querier:   querier,
	}
}

func (c *genericMergeSeriesSet) Next() bool {
	// Run in a loop because the "next" series sets may not be valid anymore.
	// If a remote querier fails, we discard all series sets from that querier.
	// If, for the current label set, all the next series sets come from
	// failed remote storage sources, we want to keep trying with the next label set.
	for {
		// Firstly advance all the current series sets.  If any of them have run out
		// we can drop them, otherwise they should be inserted back into the heap.
		for _, set := range c.currentSets {
			if set.Next() {
				heap.Push(&c.heap, set)
			}
		}
		if len(c.heap) == 0 {
			return false
		}

		// Now, pop items of the heap that have equal label sets.
		c.currentSets = nil
		c.currentLabels = c.heap[0].At().Labels()
		for len(c.heap) > 0 && labels.Equal(c.currentLabels, c.heap[0].At().Labels()) {
			set := heap.Pop(&c.heap).(genericSeriesSet)
			if c.querier != nil && c.querier.IsFailedSet(set) {
				continue
			}
			c.currentSets = append(c.currentSets, set)
		}

		// As long as the current set contains at least 1 set,
		// then it should return true.
		if len(c.currentSets) != 0 {
			break
		}
	}
	return true
}

func (c *genericMergeSeriesSet) At() Labels {
	if len(c.currentSets) == 1 {
		return c.currentSets[0].At()
	}
	series := make([]Labels, 0, len(c.currentSets))
	for _, seriesSet := range c.currentSets {
		series = append(series, seriesSet.At())
	}
	return c.mergeFunc(series...)
}

func (c *genericMergeSeriesSet) Err() error {
	for _, set := range c.sets {
		if err := set.Err(); err != nil {
			return err
		}
	}
	return nil
}

type genericSeriesSetHeap []genericSeriesSet

func (h genericSeriesSetHeap) Len() int      { return len(h) }
func (h genericSeriesSetHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h genericSeriesSetHeap) Less(i, j int) bool {
	a, b := h[i].At().Labels(), h[j].At().Labels()
	return labels.Compare(a, b) < 0
}

func (h *genericSeriesSetHeap) Push(x interface{}) {
	*h = append(*h, x.(genericSeriesSet))
}

func (h *genericSeriesSetHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// ChainingSeriesMerge returns single series from many same series by chaining samples together.
// In case of the timestamp overlap, the first overlapped sample is kept and the rest samples with the same timestamps
// are dropped. We expect the same labels for each given series.
//
// This works the best with replicated series, where data from two series are exactly the same. This does not work well
// with "almost" the same data, e.g. from 2 Prometheus HA replicas. This is fine, since from the Prometheus perspective
// this never happens.
func ChainingSeriesMerge(s ...Series) Series {
	if len(s) == 0 {
		return nil
	}
	return &chainSeries{
		labels: s[0].Labels(),
		series: s,
	}
}

type chainSeries struct {
	labels labels.Labels
	series []Series
}

func (m *chainSeries) Labels() labels.Labels {
	return m.labels
}

func (m *chainSeries) Iterator() chunkenc.Iterator {
	iterators := make([]chunkenc.Iterator, 0, len(m.series))
	for _, s := range m.series {
		iterators = append(iterators, s.Iterator())
	}
	return newChainSampleIterator(iterators)
}

// chainSampleIterator is responsible to iterate over samples from different iterators of the same time series.
// If one or more samples overlap, the first one is kept and all others with the same timestamp are dropped.
type chainSampleIterator struct {
	iterators []chunkenc.Iterator
	h         samplesIteratorHeap
}

func newChainSampleIterator(iterators []chunkenc.Iterator) chunkenc.Iterator {
	return &chainSampleIterator{
		iterators: iterators,
		h:         nil,
	}
}

func (c *chainSampleIterator) Seek(t int64) bool {
	c.h = samplesIteratorHeap{}
	for _, iter := range c.iterators {
		if iter.Seek(t) {
			heap.Push(&c.h, iter)
		}
	}
	return len(c.h) > 0
}

func (c *chainSampleIterator) At() (t int64, v float64) {
	if len(c.h) == 0 {
		panic("chainSampleIterator.At() called after .Next() returned false.")
	}

	return c.h[0].At()
}

func (c *chainSampleIterator) Next() bool {
	if c.h == nil {
		for _, iter := range c.iterators {
			if iter.Next() {
				heap.Push(&c.h, iter)
			}
		}

		return len(c.h) > 0
	}

	if len(c.h) == 0 {
		return false
	}

	currt, _ := c.At()
	for len(c.h) > 0 {
		nextt, _ := c.h[0].At()
		// All but one of the overlapping samples will be dropped.
		if nextt != currt {
			break
		}

		iter := heap.Pop(&c.h).(chunkenc.Iterator)
		if iter.Next() {
			heap.Push(&c.h, iter)
		}
	}

	return len(c.h) > 0
}

func (c *chainSampleIterator) Err() error {
	for _, iter := range c.iterators {
		if err := iter.Err(); err != nil {
			return err
		}
	}
	return nil
}

type samplesIteratorHeap []chunkenc.Iterator

func (h samplesIteratorHeap) Len() int      { return len(h) }
func (h samplesIteratorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h samplesIteratorHeap) Less(i, j int) bool {
	at, _ := h[i].At()
	bt, _ := h[j].At()
	return at < bt
}

func (h *samplesIteratorHeap) Push(x interface{}) {
	*h = append(*h, x.(chunkenc.Iterator))
}

func (h *samplesIteratorHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type verticalChunkSeriesMerger struct {
	mergeFunc VerticalSeriesMergeFunc
	labels    labels.Labels
	series    []ChunkSeries
}

// NewCompactingChunkSeriesMerger returns VerticalChunkSeriesMergeFunc that merges the same chunk series into one chunk series.
// In case of the chunk overlaps, it compacts overlapping chunks into one or more time-ordered chunks with merged data.
// Samples from overlapped chunks are merged using *series* (not chunk) vertical merge func.
// It expects the same labels for each given series.
func NewCompactingChunkSeriesMerger(mergeFunc VerticalSeriesMergeFunc) VerticalChunkSeriesMergeFunc {
	return func(s ...ChunkSeries) ChunkSeries {
		if len(s) == 0 {
			return nil
		}
		return &verticalChunkSeriesMerger{
			mergeFunc: mergeFunc,
			labels:    s[0].Labels(),
			series:    s,
		}
	}
}

func (s *verticalChunkSeriesMerger) Labels() labels.Labels {
	return s.labels
}

func (s *verticalChunkSeriesMerger) Iterator() chunks.Iterator {
	iterators := make([]chunks.Iterator, 0, len(s.series))
	for _, series := range s.series {
		iterators = append(iterators, series.Iterator())
	}
	return &verticalChunkIterator{
		mergeFunc: s.mergeFunc,
		labels:    s.labels,
		iterators: iterators,
	}
}

// verticalChunkIterator is responsible to chain chunks from different iterators of same time series.
// If time-overlapping chunks are found, they are encoded and passed to series merge and encoded again into one bigger chunk.
// TODO(bwplotka): Currently merge will compact overlapping chunks with bigger chunk, without limit. Split it: https://github.com/prometheus/tsdb/issues/670
type verticalChunkIterator struct {
	mergeFunc VerticalSeriesMergeFunc
	labels    labels.Labels
	iterators []chunks.Iterator

	h chunkIteratorHeap
}

func (c *verticalChunkIterator) At() chunks.Meta {
	if len(c.h) == 0 {
		panic("verticalChunkIterator.At() called after .Next() returned false.")
	}

	return c.h[0].At()
}

func (c *verticalChunkIterator) Next() bool {
	if c.h == nil {
		for _, iter := range c.iterators {
			if iter.Next() {
				heap.Push(&c.h, iter)
			}
		}

		return len(c.h) > 0
	}

	if len(c.h) == 0 {
		return false
	}

	// Detect the shortest chain of time-overlapped chunks.
	last := c.At()
	var overlapped []Series
	for {
		iter := heap.Pop(&c.h).(chunks.Iterator)
		if iter.Next() {
			heap.Push(&c.h, iter)
		}

		if len(c.h) == 0 {
			break
		}

		next := c.At()
		if next.MinTime > last.MaxTime {
			// No overlap with last one.
			break
		}
		overlapped = append(overlapped, &chunkToSeriesDecoder{
			labels: c.labels,
			Meta:   last,
		})
		last = next
	}

	if len(overlapped) == 0 {
		return len(c.h) > 0
	}

	// Add last, not yet included overlap.
	overlapped = append(overlapped, &chunkToSeriesDecoder{
		labels: c.labels,
		Meta:   c.At(),
	})

	// TODO(bwplotka): We could have a quick path for **exactly** the same chunks.
	// Before we do this we might need to add micro benchmark first.

	var chkSeries ChunkSeries = &seriesToChunkEncoder{Series: c.mergeFunc(overlapped...)}
	heap.Push(&c.h, chkSeries)
	return true
}

func (c *verticalChunkIterator) Err() error {
	for _, iter := range c.iterators {
		if err := iter.Err(); err != nil {
			return err
		}
	}
	return nil
}

type chunkIteratorHeap []chunks.Iterator

func (h chunkIteratorHeap) Len() int      { return len(h) }
func (h chunkIteratorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h chunkIteratorHeap) Less(i, j int) bool {
	at := h[i].At()
	bt := h[j].At()
	if at.MinTime == bt.MinTime {
		return at.MaxTime < bt.MaxTime
	}
	return at.MinTime < bt.MinTime
}

func (h *chunkIteratorHeap) Push(x interface{}) {
	*h = append(*h, x.(chunks.Iterator))
}

func (h *chunkIteratorHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
