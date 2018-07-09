package tormenta

import (
	"bytes"
	"reflect"
	"strings"

	"github.com/jpincas/gouuidv6"
)

const (
	contentKeyPrefix = "c"
	indexKeyPrefix   = "i"
	keySeparator     = "::"
)

type key struct {
	isIndex      bool
	entityType   []byte
	id           gouuidv6.UUID
	indexName    []byte
	indexContent interface{}
	exactMatch   bool
}

// newContentKey returns a key of the correct type
func newContentKey(root []byte, id ...gouuidv6.UUID) key {
	return withID(key{
		isIndex:    false,
		entityType: root,
	}, id)
}

func newIndexKey(root, indexName []byte, indexContent interface{}, id ...gouuidv6.UUID) key {
	return withID(key{
		isIndex:      true,
		entityType:   root,
		indexName:    indexName,
		indexContent: indexContent,
	}, id)
}

func newIndexMatchKey(root, indexName []byte, indexContent interface{}, id ...gouuidv6.UUID) key {
	return withID(key{
		isIndex:      true,
		exactMatch:   true,
		entityType:   root,
		indexName:    indexName,
		indexContent: indexContent,
	}, id)
}

func withID(k key, id []gouuidv6.UUID) key {
	// If an ID is specified
	if len(id) > 0 {
		k.id = id[0]
	}

	return k
}

func (k key) shouldAppendIndex() bool {
	// If index is nil, definite no
	if k.id.IsNil() {
		return false
	}

	// For non-index keys, do append
	if !k.isIndex {
		return true
	}

	// For index keys using exact matching, do append
	if k.exactMatch {
		return true
	}

	return false
}

func (k key) bytes() []byte {
	// Use either content/index key prefix
	identifierPrefix := []byte(contentKeyPrefix)
	if k.isIndex {
		identifierPrefix = []byte(indexKeyPrefix)
	}

	// Always start with identifier prefix and entity type
	toJoin := [][]byte{identifierPrefix, k.entityType}

	// For index keys, now append index name and content
	if k.isIndex {
		toJoin = append(toJoin, k.indexName, interfaceToBytes(k.indexContent))
	}

	if k.shouldAppendIndex() {
		toJoin = append(toJoin, k.id.Bytes())
	}

	return bytes.Join(toJoin, []byte(keySeparator))
}

func extractID(b []byte) (uuid gouuidv6.UUID) {
	s := bytes.Split(b, []byte(keySeparator))
	idBytes := s[len(s)-1]
	copy(uuid[:], idBytes)
	return
}

func stripID(b []byte) []byte {
	s := bytes.Split(b, []byte(keySeparator))
	return bytes.Join(s[:len(s)-1], []byte(keySeparator))
}

// compare compares two key-byte slices
func compareKeyBytes(a, b []byte, reverse bool, removeID bool) bool {
	if removeID {
		b = stripID(b)
	}

	var r int

	if !reverse {
		r = bytes.Compare(a, b)
	} else {
		r = bytes.Compare(b, a)
	}

	if r >= 0 {
		return true
	}

	return false
}

// Key construction helpers

func entityTypeAndValue(t interface{}) ([]byte, reflect.Value) {
	e := reflect.Indirect(reflect.ValueOf(t))
	return typeToKeyRoot(e.Type().String()), e
}

func typeToKeyRoot(typeSig string) []byte {
	sp := strings.Split(typeSig, ".")
	s := sp[len(sp)-1]
	s = strings.TrimPrefix(s, "*")
	s = strings.TrimPrefix(s, "[]")
	s = strings.ToLower(s)

	return []byte(s)
}
