package api

import (
	"sort"
	"strings"
	"time"
	"unsafe"
)

// metadataCacheBudget bounds retained records and accounted payload/index bytes.
// Accounting includes slice capacities and string content, with a fixed charge
// for each name-map entry. It is not an exact Go heap or process RSS measurement.
type metadataCacheBudget struct {
	bytes int64
	items int
}

type metadataNameSelection struct {
	name, fallbackPrefix string
}

func (c *Client) metadataLimitsLocked() metadataCacheBudget {
	limits := metadataCacheBudget{bytes: metadataCacheMaxBytes, items: metadataCacheMaxItems}
	if c.metadataBudget.bytes > 0 {
		limits.bytes = min(limits.bytes, c.metadataBudget.bytes)
	}
	if c.metadataBudget.items > 0 {
		limits.items = min(limits.items, c.metadataBudget.items)
	}
	return limits
}

func newMetadataEntry(parentID string, items []Item, limits metadataCacheBudget) (metadataCacheEntry, bool) {
	if len(items) > limits.items {
		return metadataCacheEntry{}, false
	}
	size := int64(len(items))*int64(unsafe.Sizeof(Item{})) + int64(unsafe.Sizeof(metadataCacheEntry{}))
	charge := func(n int64) bool {
		if n > limits.bytes || size > limits.bytes-n {
			return false
		}
		size += n
		return true
	}
	// Snapshot and retained generation maps each own a parent key.
	for range 2 {
		if !charge(int64(len(parentID))) {
			return metadataCacheEntry{}, false
		}
	}
	for _, item := range items {
		for _, field := range []string{item.ID, item.Name, item.ParentID, item.URL} {
			if !charge(int64(len(field))) {
				return metadataCacheEntry{}, false
			}
		}
	}
	entry := metadataCacheEntry{items: cloneItems(items), names: make(map[string][]int)}
	for index := range entry.items {
		item := &entry.items[index]
		// A small substring must not retain an uncharged response backing buffer.
		item.ID = strings.Clone(item.ID)
		item.Name = strings.Clone(item.Name)
		item.ParentID = strings.Clone(item.ParentID)
		item.URL = strings.Clone(item.URL)
		entry.names[item.Name] = append(entry.names[item.Name], index)
	}
	entry.sortedNames = make([]string, 0, len(entry.names))
	for name, indices := range entry.names {
		if !charge(64 + int64(cap(indices))*int64(unsafe.Sizeof(int(0)))) {
			return metadataCacheEntry{}, false
		}
		entry.sortedNames = append(entry.sortedNames, name)
	}
	if !charge(int64(cap(entry.sortedNames)) * int64(unsafe.Sizeof(""))) {
		return metadataCacheEntry{}, false
	}
	sort.Strings(entry.sortedNames)
	entry.bytes = size
	return entry, true
}

func (c *Client) removeMetadataEntryLocked(parentID string) {
	if entry, exists := c.metadataCache[parentID]; exists {
		c.metadataBytes -= entry.bytes
		c.metadataItems -= len(entry.items)
		delete(c.metadataCache, parentID)
	}
}

func (c *Client) cachedMetadataEntryLocked(parentID string, globalGeneration, parentGeneration uint64) (metadataCacheEntry, bool) {
	now := time.Now()
	c.pruneMetadataCacheLocked(now)
	entry, ok := c.metadataCache[parentID]
	return entry, ok && entry.globalGeneration == globalGeneration && entry.parentGeneration == parentGeneration && now.Before(entry.expires)
}

func selectCachedMetadata(entry metadataCacheEntry, selection *metadataNameSelection) []Item {
	if selection == nil {
		return cloneItems(entry.items)
	}
	indices := entry.names[selection.name]
	if len(indices) == 0 && selection.fallbackPrefix != "" {
		start := sort.SearchStrings(entry.sortedNames, selection.fallbackPrefix)
		for _, name := range entry.sortedNames[start:] {
			if !strings.HasPrefix(name, selection.fallbackPrefix) {
				break
			}
			indices = append(indices, entry.names[name]...)
		}
		// Prefix groups are name-sorted; callers retain provider listing order.
		sort.Ints(indices)
	}
	items := make([]Item, len(indices))
	for index, source := range indices {
		items[index] = entry.items[source]
	}
	return items
}

func selectListedMetadata(items []Item, selection *metadataNameSelection) []Item {
	if selection == nil {
		return cloneItems(items)
	}
	var exact, fallback []Item
	for _, item := range items {
		if item.Name == selection.name {
			exact = append(exact, item)
		} else if selection.fallbackPrefix != "" && strings.HasPrefix(item.Name, selection.fallbackPrefix) {
			fallback = append(fallback, item)
		}
	}
	if len(exact) != 0 {
		return exact
	}
	return fallback
}
