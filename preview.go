// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
	"time"
)

// Vorschaubilder der GIF-Suche werden über den Server geladen (Besucher bauen keine Verbindung
// zu GIPHY/Tenor/Beacon auf). Damit das nicht zur Last wird: Zwischenspeicher für dieselben
// Vorschauen, höchstens vier gleichzeitige Downloads und ein Limit je IP (im Handler).

const (
	previewCacheMax  = 32 << 20 // Bytes im Zwischenspeicher
	previewCacheItem = 4 << 20  // größere Vorschauen werden nicht zwischengespeichert
	previewTTL       = 30 * time.Minute
)

type previewItem struct {
	data []byte
	ct   string
	at   time.Time
}

type previewCache struct {
	mu    sync.Mutex
	items map[string]*previewItem
	order []string // älteste zuerst
	size  int
	slots chan struct{}
}

func newPreviewCache() *previewCache {
	return &previewCache{items: map[string]*previewItem{}, slots: make(chan struct{}, 4)}
}

func (c *previewCache) get(id string) (*previewItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	if !ok || time.Since(it.at) > previewTTL {
		return nil, false
	}
	return it, true
}

func (c *previewCache) put(id string, data []byte, ct string) {
	if len(data) > previewCacheItem {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[id]; ok {
		c.size -= len(old.data)
	} else {
		c.order = append(c.order, id)
	}
	c.items[id] = &previewItem{data: data, ct: ct, at: time.Now()}
	c.size += len(data)
	for c.size > previewCacheMax && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if it, ok := c.items[oldest]; ok {
			c.size -= len(it.data)
			delete(c.items, oldest)
		}
	}
}

// acquire wartet höchstens 10 Sekunden auf einen freien Download-Platz.
func (c *previewCache) acquire() (func(), bool) {
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case c.slots <- struct{}{}:
		return func() { <-c.slots }, true
	case <-t.C:
		return nil, false
	}
}
