// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/iterators/chunk_merge_iterator.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package iterators

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/grafana/mimir/pkg/storage/chunk"
)

type chunkMergeIterator struct {
	its []*nonOverlappingIterator
	h   seriesIteratorHeap

	currTime  int64
	currValue float64
	currErr   error
}

// NewChunkMergeIterator creates a chunkenc.Iterator for a set of chunks.
func NewChunkMergeIterator(cs []chunk.Chunk, _, _ model.Time) chunkenc.Iterator {
	its := buildIterators(cs)
	c := &chunkMergeIterator{
		currTime: -1,
		its:      its,
		h:        make(seriesIteratorHeap, 0, len(its)),
	}

	for _, iter := range c.its {
		valType := iter.Next()
		// TODO for native histograms: allow all types
		if valType == chunkenc.ValFloat {
			c.h = append(c.h, iter)
			continue
		}
		if valType != chunkenc.ValNone {
			c.currErr = fmt.Errorf("unsupported value type %v", valType)
			break
		}

		if err := iter.Err(); err != nil {
			c.currErr = err
			break
		}
	}

	heap.Init(&c.h)
	return c
}

// Build a list of lists of non-overlapping chunk iterators.
func buildIterators(cs []chunk.Chunk) []*nonOverlappingIterator {
	chunks := make([]*chunkIterator, len(cs))
	for i := range cs {
		chunks[i] = &chunkIterator{
			Chunk: cs[i],
			it:    cs[i].Data.NewIterator(nil),
		}
	}
	sort.Sort(byFrom(chunks))

	chunkLists := [][]*chunkIterator{}
outer:
	for _, chunk := range chunks {
		for i, chunkList := range chunkLists {
			if chunkList[len(chunkList)-1].Through.Before(chunk.From) {
				chunkLists[i] = append(chunkLists[i], chunk)
				continue outer
			}
		}
		chunkLists = append(chunkLists, []*chunkIterator{chunk})
	}

	its := make([]*nonOverlappingIterator, 0, len(chunkLists))
	for _, chunkList := range chunkLists {
		its = append(its, newNonOverlappingIterator(chunkList))
	}
	return its
}

func (c *chunkMergeIterator) Seek(t int64) chunkenc.ValueType {
	if c.currErr != nil {
		return chunkenc.ValNone
	}
	c.h = c.h[:0]

	for _, iter := range c.its {
		valType := iter.Seek(t)
		// TODO for native histograms: allow all types
		if valType == chunkenc.ValFloat {
			c.h = append(c.h, iter)
			continue
		}
		if valType != chunkenc.ValNone {
			c.currErr = fmt.Errorf("unsupported value type %v", valType)
			return chunkenc.ValNone
		}

		if err := iter.Err(); err != nil {
			c.currErr = err
			return chunkenc.ValNone
		}
	}

	heap.Init(&c.h)

	if len(c.h) > 0 {
		c.currTime, c.currValue = c.h[0].At()
		return chunkenc.ValFloat
	}

	return chunkenc.ValNone
}

func (c *chunkMergeIterator) Next() chunkenc.ValueType {
	if len(c.h) == 0 || c.currErr != nil {
		return chunkenc.ValNone
	}

	lastTime := c.currTime
	for c.currTime == lastTime && len(c.h) > 0 {
		c.currTime, c.currValue = c.h[0].At()

		if c.h[0].Next() != chunkenc.ValNone {
			heap.Fix(&c.h, 0)
			continue
		}

		iter := heap.Pop(&c.h).(chunkenc.Iterator)
		if err := iter.Err(); err != nil {
			c.currErr = err
			return chunkenc.ValNone
		}
	}

	if c.currTime != lastTime {
		// TODO for native histograms: return the correct type
		return chunkenc.ValFloat
	}
	return chunkenc.ValNone
}

func (c *chunkMergeIterator) At() (t int64, v float64) {
	return c.currTime, c.currValue
}

// Histogram support isn't complete yet
func (c *chunkMergeIterator) AtHistogram() (int64, *histogram.Histogram) {
	panic(errors.New("chunkMergeIterator: AtHistogram is not implemented"))
}

// Histogram suppport isn't complete yet
func (c *chunkMergeIterator) AtFloatHistogram() (int64, *histogram.FloatHistogram) {
	panic(errors.New("chunkMergeIterator: AtFloatHistogram is not implemented"))
}

func (c *chunkMergeIterator) AtT() int64 {
	return c.currTime
}

func (c *chunkMergeIterator) Err() error {
	return c.currErr
}

type extraIterator interface {
	chunkenc.Iterator
	AtTime() int64
}

type seriesIteratorHeap []extraIterator

func (h *seriesIteratorHeap) Len() int      { return len(*h) }
func (h *seriesIteratorHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

func (h *seriesIteratorHeap) Less(i, j int) bool {
	iT := (*h)[i].AtTime()
	jT := (*h)[j].AtTime()
	return iT < jT
}

func (h *seriesIteratorHeap) Push(x interface{}) {
	*h = append(*h, x.(extraIterator))
}

func (h *seriesIteratorHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type byFrom []*chunkIterator

func (b byFrom) Len() int           { return len(b) }
func (b byFrom) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byFrom) Less(i, j int) bool { return b[i].From < b[j].From }

type nonOverlappingIterator struct {
	curr   int
	chunks []*chunkIterator
}

// newNonOverlappingIterator returns a single iterator over an slice of sorted,
// non-overlapping iterators.
func newNonOverlappingIterator(chunks []*chunkIterator) *nonOverlappingIterator {
	return &nonOverlappingIterator{
		chunks: chunks,
	}
}

func (it *nonOverlappingIterator) Seek(t int64) chunkenc.ValueType {
	for ; it.curr < len(it.chunks); it.curr++ {
		if v := it.chunks[it.curr].Seek(t); v != chunkenc.ValNone {
			return v
		}
	}

	return chunkenc.ValNone
}

func (it *nonOverlappingIterator) Next() chunkenc.ValueType {
	for it.curr < len(it.chunks) {
		valType := it.chunks[it.curr].Next()
		if valType != chunkenc.ValNone {
			return valType
		}
		it.curr++
	}

	return chunkenc.ValNone
}

func (it *nonOverlappingIterator) AtTime() int64 {
	return it.chunks[it.curr].AtTime()
}

func (it *nonOverlappingIterator) At() (int64, float64) {
	return it.chunks[it.curr].At()
}

func (it *nonOverlappingIterator) AtHistogram() (int64, *histogram.Histogram) {
	return it.chunks[it.curr].AtHistogram()
}

func (it *nonOverlappingIterator) AtFloatHistogram() (int64, *histogram.FloatHistogram) {
	return it.chunks[it.curr].AtFloatHistogram()
}

func (it *nonOverlappingIterator) AtT() int64 {
	return it.chunks[it.curr].AtT()
}

func (it *nonOverlappingIterator) Err() error {
	return nil
}
