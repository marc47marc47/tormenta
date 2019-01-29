package tormenta

import (
	"bytes"
	"fmt"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/jpincas/gouuidv6"
)

type Query struct {
	// Connection to BadgerDB
	db DB

	// The entity type being searched
	keyRoot []byte

	// Target is the pointer passed into the Query where results will be set
	target interface{}

	// Limit number of returned results
	limit int

	// Offet - start returning results N entities from the beginning
	// offsetCounter used to track the offset
	offset, offsetCounter int

	// Reverse fullStruct of searching and returned results
	reverse bool

	// Is this a 'first only' search
	first bool

	// The start and end points of the index range search
	start, end interface{}

	// From and To dates for the search
	from, to gouuidv6.UUID

	// If this is an index search, this is the name of the index
	indexName []byte

	// Is this an index Query
	isIndexQuery bool

	// Is this a 'starts with' index query
	isStartsWithQuery bool

	// Is this a count only search
	countOnly bool

	// A placeholders for errors to be passed down through the Query
	err error

	// Ranges and comparision key
	seekFrom, validTo, compareTo []byte

	// Is this an aggregation Query?
	isAggQuery bool
	aggTarget  interface{}

	// Is already prepared?
	isReversePrepared bool

	// Pass-through context
	ctx map[string]interface{}

	// Ids for retrieval
	ids idList

	combinedQuery bool
}

func (db DB) newQuery(target interface{}, first bool) *Query {
	// Create the base Query
	q := &Query{
		db:      db,
		keyRoot: KeyRoot(target),
		target:  target,
	}

	// If this is a 'first only' Query
	if first {
		q.limit = 1
		q.first = true
	}

	// Start with blank context
	q.ctx = make(map[string]interface{})

	return q
}

func (q Query) getIteratorOptions(getValues bool) badger.IteratorOptions {
	options := badger.DefaultIteratorOptions
	options.Reverse = q.reverse
	options.PrefetchValues = getValues
	return options
}

func (q Query) isExactIndexMatchSearch() bool {
	return q.start == q.end && q.start != nil && q.end != nil
}

func (q Query) isIndexRangeSearch() bool {
	return q.start != q.end && !q.isStartsWithQuery
}

func (q Query) shouldGetValues() bool {
	// For index queries or count only queries, don't get values
	if q.isIndexQuery || q.countOnly {
		return false
	}

	return true
}

func (q Query) shouldStripKeyID() bool {
	// Regular queries never need to have ID stripped
	if !q.isIndexQuery {
		return false
	}

	// Index queries which are exact match AND have a 'to' clause
	// also never need to have ID stripped
	if q.isExactIndexMatchSearch() && !q.to.IsNil() {
		return false
	}

	return true
}

func (q Query) isEndOfRange(it *badger.Iterator) bool {
	key := it.Item().Key()

	if q.isIndexQuery {
		return q.end != nil && compareKeyBytes(q.compareTo, key, q.reverse, q.shouldStripKeyID())
	}

	return !q.to.IsNil() && compareKeyBytes(q.compareTo, key, q.reverse, q.shouldStripKeyID())
}

func (q Query) isLimitMet() bool {
	return q.limit > 0 && len(q.ids) >= q.limit
}

func (q Query) endIteration(it *badger.Iterator) bool {
	if it.ValidForPrefix(q.validTo) {
		if q.isLimitMet() || q.isEndOfRange(it) {
			return false
		}

		return true
	}

	return false
}

func (q Query) aggregate(item *badger.Item) {
	// TODO: super inefficient to do this every time
	switch q.aggTarget.(type) {
	case *int32:
		acc := *q.aggTarget.(*int32)
		extractIndexValue(item.Key(), q.aggTarget)
		*q.aggTarget.(*int32) = acc + *q.aggTarget.(*int32)
	case *float64:
		acc := *q.aggTarget.(*float64)
		extractIndexValue(item.Key(), q.aggTarget)
		*q.aggTarget.(*float64) = acc + *q.aggTarget.(*float64)
	}
}

func (q *Query) setRanges() {
	var seekFrom, validTo, compareTo []byte

	// For reverse queries, append the byte 0xFF to get inclusive results
	// See Badger issue: https://github.com/dgraph-io/badger/issues/347
	// Also, flick-flack start/end and from/to
	// to provide a standardised user API
	// CAUTION: we don't want to do this more than once for a query,
	// so just in case this is a query being run for a second time,
	// we maintain the flag 'is reverse prepared' to guard against this
	if q.reverse && !q.isReversePrepared {
		seekFrom = append(seekFrom, 0xFF)

		tempEnd := q.end
		q.end = q.start
		q.start = tempEnd

		tempTo := q.to
		q.to = q.from
		q.from = tempTo

		q.isReversePrepared = true
	}

	if q.isIndexQuery && q.isExactIndexMatchSearch() {
		// For index searches with exact match
		seekFrom = newIndexMatchKey(q.keyRoot, q.indexName, q.start, q.from).bytes()
		validTo = newIndexMatchKey(q.keyRoot, q.indexName, q.end).bytes()
		compareTo = newIndexMatchKey(q.keyRoot, q.indexName, q.end, q.to).bytes()
	} else if q.isIndexQuery {
		// For regular index searches
		seekFrom = newIndexKey(q.keyRoot, q.indexName, q.start).bytes()
		validTo = newIndexKey(q.keyRoot, q.indexName, nil).bytes()
		compareTo = newIndexKey(q.keyRoot, q.indexName, q.end).bytes()
	} else {
		seekFrom = newContentKey(q.keyRoot, q.from).bytes()
		validTo = newContentKey(q.keyRoot).bytes()
		compareTo = newContentKey(q.keyRoot, q.to).bytes()
	}

	q.seekFrom = seekFrom
	q.validTo = validTo
	q.compareTo = compareTo
}

func (q *Query) resetQuery() {
	// Counter should always be reset before executing a Query.
	// Just in case a Query is built then executed twice.
	q.offsetCounter = q.offset

	// Also, id list should be reset -
	// UNLESS this is an 'already executed' query!!!
	if !q.combinedQuery {
		q.ids = idList{}
	}
}

func (q *Query) setFromToIfEmpty() {
	// For index range searches - we don't do this,
	// so exit right away
	if q.isIndexRangeSearch() {
		return
	}

	// If 'from' or 'to' have not been specified manually by the user,
	// then we set them to the 'widest' times possible,
	// i.e. 'between beginning of time' and 'now'
	// If we don't do this, then some searches work OK, but particuarly reversed searches
	// can experience strange behaviour (namely returned 0 results), because the iteration
	// ends up starting from the end of the list.
	// Another side-effect of not doing this is that exact match string searches would become 'starts with' searches.  We might want that behaviour though, so we include a check for this type of search below

	t1 := time.Time{}
	t2 := time.Now()

	// Reverse the endpoints of the range for 'reverse' searches
	// if q.reverse {
	// 	temp := t1
	// 	t1 = t2
	// 	t2 = temp
	// }

	if q.from.IsNil() {
		// If we are doing a 'starts with' query,
		// then we DON'T want to set the from point
		// This magically gives us 'starts with'
		// instead of exact match,
		// BUT - this trick only works for forward searches,
		// not 'reverse' searches,
		// so there is a protection in the query preparation
		if !q.isStartsWithQuery {
			q.From(t1)
		}
	}

	if q.to.IsNil() {
		q.To(t2)
	}
}

func (q *Query) prepareQuery() {
	// 'starts with' type query doesn't work with reverse
	// so switch it back to a regular search
	if q.isIndexQuery && q.isStartsWithQuery && q.reverse {
		q.reverse = false
	}

	// Lowercase index name
	if q.isIndexQuery {
		q.indexName = bytes.ToLower(q.indexName)
	}

	q.setFromToIfEmpty()
	q.setRanges()
}

func (q *Query) queryIDs() error {
	q.prepareQuery()
	q.resetQuery()

	// Now, if during the query planning and preparation,
	// something has gone wrong and an error has been set on the query,
	// we'll return right here and now
	if q.err != nil {
		return q.err
	}

	// Iterate through records according to calcuted range limits
	err := q.db.KV.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(q.getIteratorOptions(q.shouldGetValues()))
		defer it.Close()

		// Start iteration
		for it.Seek(q.seekFrom); q.endIteration(it); it.Next() {
			// If this is a 'range index' type Query
			// that ALSO has a date range, the procedure is a little more complicated
			// compared to an exact index match.
			// Since the start/end points of the iteration focus on the index, e.g. E-J (alphabetical index)
			// we need to manually check all the keys and reject those that don't fit the date range
			if q.isIndexQuery && !q.isExactIndexMatchSearch() {
				key := extractID(it.Item().Key())
				if keyIsOutsideDateRange(key, q.from, q.to) {
					continue
				}
			}

			// Skip the first N entities according to the specified offset
			if q.offsetCounter > 0 {
				q.offsetCounter--
				continue
			}

			// For non-count-only queries, we'll actually get the record
			// How this is done depends on whether this is an index-based search or not
			item := it.Item()
			q.ids = append(q.ids, extractID(item.Key()))

			if q.isAggQuery {
				q.aggregate(item)
			}

			// If this is a first-only search, break out of the iteration now
			if q.first {
				return nil
			}
		}

		return nil
	})

	return err
}

func (q *Query) execute() (int, error) {
	// Combined queries have already had their IDs retrieved,
	// so we can skip this step
	if !q.combinedQuery {
		// Step 1: get the IDs returned for this query
		err := q.queryIDs()
		if err != nil {
			return 0, err
		}
	}

	// For count-only, there's nothing more to do
	if q.countOnly {
		return len(q.ids), nil
	}

	// Step 2:
	// For 'First' type queries
	if q.first {
		// For 'first' queries, we should check that there is at least 1 record found
		// before trying to set it
		if len(q.ids) == 0 {
			return 0, nil
		}

		// db.get ususally takes a 'Record', so we need to set a new one up
		// and then set the result of get to the target aftwards
		record := newRecord(q.target)
		id := q.ids[0]
		if found, err := q.db.get(record, q.ctx, id); err != nil {
			return 0, err
		} else if !found {
			return 0, fmt.Errorf("Could not retrieve record with id: %v", id)
		}

		setSingleResultOntoTarget(q.target, record)
		return 1, nil
	}

	// For non 'first' type queries
	// In the case of combined queries, the IDs are going to be mixed up because they come
	// from a combination of multiple queries, so lets remedy that first
	if q.combinedQuery {
		q.ids.sort(q.reverse)
	}

	n, err := q.db.GetIDsWithContext(q.target, q.ctx, q.ids...)
	if err != nil {
		return 0, err
	}

	return n, nil
}