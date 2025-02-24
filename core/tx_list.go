// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"container/heap"
	"math/big"
	"sort"

	"github.com/gochain/gochain/v4/core/types"
)

// nonceHeap is a heap.Interface implementation over 64bit unsigned integers for
// retrieving sorted transactions from the possibly gapped future queue.
type nonceHeap []uint64

func (h nonceHeap) Len() int           { return len(h) }
func (h nonceHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h nonceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *nonceHeap) Push(x interface{}) {
	*h = append(*h, x.(uint64))
}

func (h *nonceHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// txSortedMap is a nonce->transaction hash map with a heap based index to allow
// iterating over the contents in a nonce-incrementing way.
type txSortedMap struct {
	items map[uint64]*types.Transaction // Hash map storing the transaction data
	index *nonceHeap                    // Heap of nonces of all the stored transactions (non-strict mode)
	cache types.Transactions            // Cache of the transactions already sorted
}

// newTxSortedMap creates a new nonce-sorted transaction map.
func newTxSortedMap() *txSortedMap {
	return &txSortedMap{
		items: make(map[uint64]*types.Transaction),
		index: &nonceHeap{},
	}
}

// Get retrieves the current transactions associated with the given nonce.
func (m *txSortedMap) Get(nonce uint64) *types.Transaction {
	return m.items[nonce]
}

// Put inserts a new transaction into the map, also updating the map's nonce
// index. If a transaction already exists with the same nonce, it's overwritten.
func (m *txSortedMap) Put(tx *types.Transaction) {
	nonce := tx.Nonce()
	if m.items[nonce] == nil {
		heap.Push(m.index, nonce)
	}
	m.items[nonce], m.cache = tx, nil
}

// Forward removes all transactions from the map with a nonce lower than the
// provided threshold. Every removed transaction is passed to fn for any post-removal
// maintenance.
func (m *txSortedMap) Forward(threshold uint64, fn func(*types.Transaction)) {
	var removed int
	// Pop off heap items until the threshold is reached
	for m.index.Len() > 0 && (*m.index)[0] < threshold {
		nonce := heap.Pop(m.index).(uint64)
		item := m.items[nonce]
		delete(m.items, nonce)
		fn(item)
		removed++
	}
	// If we had a cached order, shift the front
	if m.cache != nil {
		m.cache = m.cache[removed:]
	}
}

// Filter iterates over the list of transactions calling filter, removing and calling removed for each match. If strict
// is true, then all txs with nonces higher than the first match are removed and passed to invalid.
func (m *txSortedMap) Filter(filter func(*types.Transaction) bool, strict bool, removed, invalid func(*types.Transaction)) {
	if strict {
		// Iterate in order so we can slice off the higher nonces.
		m.ensureCache()
		for i, tx := range m.cache {
			if !filter(tx) {
				continue
			}
			delete(m.items, tx.Nonce())
			removed(tx)

			if len(m.cache) > i+1 {
				for _, tx := range m.cache[i+1:] {
					delete(m.items, tx.Nonce())
					invalid(tx)
				}
			}

			m.cache = m.cache[:i]

			// Rebuild heap.
			*m.index = make([]uint64, 0, len(m.items))
			for nonce := range m.items {
				*m.index = append(*m.index, nonce)
			}
			heap.Init(m.index)

			return
		}
		return
	}

	var matched bool
	for nonce, tx := range m.items {
		if !filter(tx) {
			continue
		}
		matched = true
		delete(m.items, nonce)
		removed(tx)
	}

	// If transactions were removed, the heap and cache are ruined
	if matched {
		*m.index = make([]uint64, 0, len(m.items))
		for nonce := range m.items {
			*m.index = append(*m.index, nonce)
		}
		heap.Init(m.index)

		m.cache = nil
	}
}

// Cap places a hard limit on the number of items, removing and calling removed with each transaction
// exceeding that limit.
func (m *txSortedMap) Cap(threshold int, removed func(*types.Transaction)) {
	// Short circuit if the number of items is under the limit.
	if len(m.items) <= threshold {
		return
	}

	// Resort the heap to drop the highest nonce'd transactions.
	var drops int
	sort.Sort(*m.index)
	for size := len(m.items); size > threshold; size-- {
		item := m.items[(*m.index)[size-1]]
		delete(m.items, (*m.index)[size-1])
		removed(item)
		drops++
	}
	*m.index = (*m.index)[:threshold]
	// Restore the heap.
	heap.Init(m.index)

	// If we had a cache, shift the back
	if m.cache != nil {
		m.cache = m.cache[:len(m.cache)-drops]
	}
}

// Remove deletes a transaction from the maintained map, returning whether the transaction was found. If strict is true
// then it will also remove invalidated txs (higher than nonce) and call invalid for each one.
func (m *txSortedMap) Remove(nonce uint64, strict bool, invalid func(*types.Transaction)) bool {
	// Short circuit if no transaction is present
	_, ok := m.items[nonce]
	if !ok {
		return false
	}
	m.ensureCache()
	delete(m.items, nonce)
	i := sort.Search(len(m.cache), func(i int) bool {
		return m.cache[i].Nonce() >= nonce
	})

	if !strict {
		// Repair the cache and heap.
		copy(m.cache[i:], m.cache[i+1:])
		m.cache = m.cache[:len(m.cache)-1]
		for i := 0; i < m.index.Len(); i++ {
			if (*m.index)[i] == nonce {
				heap.Remove(m.index, i)
				break
			}
		}
		return true
	}

	// Remove invalidated.
	for _, tx := range m.cache[i+1:] {
		delete(m.items, tx.Nonce())
		invalid(tx)
	}

	// Repair the cache and heap.
	m.cache = m.cache[:i]
	*m.index = make([]uint64, 0, len(m.items))
	for nonce := range m.items {
		*m.index = append(*m.index, nonce)
	}
	heap.Init(m.index)

	return true
}

// Ready iterates over a sequentially increasing list of transactions that are ready for processing, removing
// and calling fn for each one.
//
// Note, all transactions with nonces lower than start will also be included to
// prevent getting into and invalid state. This is not something that should ever
// happen but better to be self correcting than failing!
func (m *txSortedMap) Ready(start uint64, fn func(*types.Transaction)) {
	// Short circuit if no transactions are available
	if m.index.Len() == 0 || (*m.index)[0] > start {
		return
	}
	if m.cache == nil {
		for next := (*m.index)[0]; m.index.Len() > 0 && (*m.index)[0] == next; next++ {
			heap.Pop(m.index)
			item := m.items[next]
			delete(m.items, next)
			fn(item)
		}
		return
	}
	next := m.cache[0].Nonce()
	for i, item := range m.cache {
		nonce := item.Nonce()
		if nonce != next {
			// Update cache.
			m.cache = m.cache[i:]
			break
		}
		delete(m.items, nonce)
		fn(item)
		next++
	}
	// Rebuild heap.
	*m.index = make([]uint64, 0, len(m.items))
	for nonce := range m.items {
		*m.index = append(*m.index, nonce)
	}
	heap.Init(m.index)
}

// Len returns the length of the transaction map.
func (m *txSortedMap) Len() int {
	return len(m.items)
}

// Flatten creates a nonce-sorted slice of transactions based on the loosely
// sorted internal representation. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (m *txSortedMap) Flatten() types.Transactions {
	m.ensureCache()
	// Copy the cache to prevent accidental modifications
	txs := make(types.Transactions, len(m.cache))
	copy(txs, m.cache)
	return txs
}

// ForLast calls fn with each of the last n txs in nonce order. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (m *txSortedMap) ForLast(n int, fn func(*types.Transaction)) {
	m.ensureCache()
	i := len(m.cache) - n
	if i < 0 {
		i = 0
	}
	for _, tx := range m.cache[i:] {
		delete(m.items, tx.Nonce())
		fn(tx)
	}
	m.cache = m.cache[:i]

	// Rebuild heap.
	*m.index = make([]uint64, 0, len(m.items))
	for nonce := range m.items {
		*m.index = append(*m.index, nonce)
	}
	heap.Init(m.index)
}

// Last returns the highest nonce tx. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (m *txSortedMap) Last() *types.Transaction {
	m.ensureCache()
	return m.cache[len(m.cache)-1]
}

func (m *txSortedMap) ensureCache() {
	// If the sorting was not cached yet, create and cache it
	if m.cache == nil {
		m.cache = make(types.Transactions, 0, len(m.items))
		for _, tx := range m.items {
			m.cache = append(m.cache, tx)
		}
		sort.Sort(types.TxByNonce(m.cache))
	}
}

// txList is a "list" of transactions belonging to an account, sorted by account
// nonce. The same type can be used both for storing contiguous transactions for
// the executable/pending queue; and for storing gapped transactions for the non-
// executable/future queue, with minor behavioral changes.
type txList struct {
	strict bool         // Whether nonces are strictly continuous or not
	txs    *txSortedMap // Heap indexed sorted hash map of the transactions

	costcap *big.Int // Price of the highest costing transaction (reset only if exceeds balance)
	gascap  uint64   // Gas limit of the highest spending transaction (reset only if exceeds block limit)
}

// newTxList create a new transaction list for maintaining nonce-indexable fast,
// gapped, sortable transaction lists.
func newTxList(strict bool) *txList {
	return &txList{
		strict:  strict,
		txs:     newTxSortedMap(),
		costcap: new(big.Int),
	}
}

// Overlaps returns whether the transaction specified has the same nonce as one
// already contained within the list.
func (l *txList) Overlaps(tx *types.Transaction) bool {
	return l.txs.Get(tx.Nonce()) != nil
}

// Add tries to insert a new transaction into the list, returning whether the
// transaction was accepted, and if yes, any previous transaction it replaced.
//
// If the new transaction is accepted into the list, the lists' cost and gas
// thresholds are also potentially updated.
func (l *txList) Add(tx *types.Transaction, priceBump uint64) (bool, *types.Transaction) {
	// If there's an older better transaction, abort
	old := l.txs.Get(tx.Nonce())
	if old != nil {
		threshold := new(big.Int).Div(new(big.Int).Mul(old.GasPrice(), big.NewInt(100+int64(priceBump))), big.NewInt(100))
		// Have to ensure that the new gas price is higher than the old gas
		// price as well as checking the percentage threshold to ensure that
		// this is accurate for low (Wei-level) gas price replacements
		if old.CmpGasPriceTx(tx) >= 0 || tx.CmpGasPrice(threshold) < 0 {
			return false, nil
		}
	}
	// Otherwise overwrite the old transaction with the current one
	l.add(tx)
	return true, old
}

func (l *txList) add(tx *types.Transaction) {
	l.txs.Put(tx)
	if cost := tx.Cost(); l.costcap.Cmp(cost) < 0 {
		l.costcap = cost
	}
	if gas := tx.Gas(); l.gascap < gas {
		l.gascap = gas
	}
}

// Forward removes all transactions from the list with a nonce lower than the
// provided threshold. Every removed transaction is passed to fn for any post-removal
// maintenance.
func (l *txList) Forward(threshold uint64, fn func(*types.Transaction)) {
	l.txs.Forward(threshold, fn)
}

// Filter removes all transactions from the list with a cost or gas limit higher
// than the provided thresholds. Every removed transaction is returned for any
// post-removal maintenance. Strict-mode invalidated transactions are also
// returned.
//
// This method uses the cached costcap and gascap to quickly decide if there's even
// a point in calculating all the costs or if the balance covers all. If the threshold
// is lower than the costgas cap, the caps will be reset to a new high after removing
// the newly invalidated transactions.
func (l *txList) Filter(costLimit *big.Int, gasLimit uint64, removed, invalid func(*types.Transaction)) {
	// If all transactions are below the threshold, short circuit
	if l.costcap.Cmp(costLimit) <= 0 && l.gascap <= gasLimit {
		return
	}
	l.costcap = new(big.Int).Set(costLimit) // Lower the caps to the thresholds
	l.gascap = gasLimit

	filter := func(tx *types.Transaction) bool {
		return tx.Cost().Cmp(costLimit) > 0 || tx.Gas() > gasLimit
	}
	l.txs.Filter(filter, l.strict, removed, invalid)
}

// Cap places a hard limit on the number of items, removing and calling removed with each transaction
// exceeding that limit.
func (l *txList) Cap(threshold int, removed func(*types.Transaction)) {
	l.txs.Cap(threshold, removed)
}

// Remove deletes a transaction from the maintained list, returning whether the
// transaction was found, and also calling invalid with each transaction invalidated due to
// the deletion (strict mode only).
func (l *txList) Remove(tx *types.Transaction, invalid func(*types.Transaction)) bool {
	return l.txs.Remove(tx.Nonce(), l.strict, invalid)
}

// Ready iterates over a sequentially increasing list of transactions that are ready for processing, removing
// and calling fn for each one.
//
// Note, all transactions with nonces lower than start will also be included to
// prevent getting into an invalid state. This is not something that should ever
// happen but better to be self correcting than failing!
func (l *txList) Ready(start uint64, fn func(*types.Transaction)) {
	l.txs.Ready(start, fn)
}

// Len returns the length of the transaction list.
func (l *txList) Len() int {
	return l.txs.Len()
}

// Empty returns whether the list of transactions is empty or not.
func (l *txList) Empty() bool {
	return l.Len() == 0
}

// Flatten creates a nonce-sorted slice of transactions based on the loosely
// sorted internal representation. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (l *txList) Flatten() types.Transactions {
	return l.txs.Flatten()
}

// ForLast calls fn with each of the last n txs in nonce order. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (l *txList) ForLast(n int, fn func(*types.Transaction)) {
	l.txs.ForLast(n, fn)
}

// Last returns the highest nonce tx. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (l *txList) Last() *types.Transaction {
	return l.txs.Last()
}
