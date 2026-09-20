package filters

import (
	"github.com/bits-and-blooms/bloom/v3"
	"github.com/seiflotfy/cuckoofilter"
)

// ToolFilter defines the interface for our fast-fail sentinels.
type ToolFilter interface {
	Add(item string)
	Test(item string) bool
	Delete(item string) bool
}

// BloomToolFilter wraps a standard Bloom filter.
// Note: Bloom filters do not support deletion.
type BloomToolFilter struct {
	filter *bloom.BloomFilter
}

func NewBloomToolFilter(expectedElements uint) *BloomToolFilter {
	// 1% false positive rate
	return &BloomToolFilter{
		filter: bloom.NewWithEstimates(expectedElements, 0.01),
	}
}

func (b *BloomToolFilter) Add(item string) {
	b.filter.AddString(item)
}

func (b *BloomToolFilter) Test(item string) bool {
	return b.filter.TestString(item)
}

func (b *BloomToolFilter) Delete(item string) bool {
	// Bloom filters do not support deletion natively without counting.
	return false
}

// CuckooToolFilter wraps a Cuckoo filter.
// Cuckoo filters support deletion.
type CuckooToolFilter struct {
	filter *cuckoo.Filter
}

func NewCuckooToolFilter(expectedElements uint) *CuckooToolFilter {
	// The Cuckoo filter requires capacity. If it fills up, it might fail to insert,
	// but cuckoofilter library handles basic resizing or we can just over-provision.
	// We'll use the provided capacity directly.
	// Make sure we have at least some capacity.
	if expectedElements < 1000 {
		expectedElements = 1000
	}
	return &CuckooToolFilter{
		filter: cuckoo.NewFilter(expectedElements),
	}
}

func (c *CuckooToolFilter) Add(item string) {
	c.filter.InsertUnique([]byte(item))
}

func (c *CuckooToolFilter) Test(item string) bool {
	return c.filter.Lookup([]byte(item))
}

func (c *CuckooToolFilter) Delete(item string) bool {
	return c.filter.Delete([]byte(item))
}
