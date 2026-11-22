package hashstore

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// objectMeta is one logical body: keyed by SHA-256, carrying the SHA-1 alias,
// its logical byte size and the access sequence that defines LRU order.
type objectMeta struct {
	SHA256 string
	SHA1   string
	Size   int64
	At     uint64
}

var (
	objectsBucket = []byte("objects")
	aliasesBucket = []byte("aliases")
	pendingBucket = []byte("pending")
)

const recordLen = 8 + 8 + 20 // size, access sequence, sha1

// index is the durable side of the store: capacity accounting (size), the
// SHA-1 alias and the LRU order (access sequence). It is only touched while the
// store mutex is held, so the sequence counter needs no atomics.
type index struct {
	db  *bolt.DB
	seq uint64
}

// indexLockTimeout bounds how long opening waits for another process that holds
// the bbolt lock. It is a variable only so tests can shorten the wait.
var indexLockTimeout = time.Second

func openIndex(path string) (*index, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: indexLockTimeout})
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	ix := &index{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{objectsBucket, aliasesBucket, pendingBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init index: %w", err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(objectsBucket).ForEach(func(_, v []byte) error {
			if len(v) != recordLen {
				return nil
			}
			if at := binary.BigEndian.Uint64(v[8:16]); at > ix.seq {
				ix.seq = at
			}
			return nil
		})
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read index: %w", err)
	}
	return ix, nil
}

func (ix *index) Close() error { return ix.db.Close() }

// nextSeq returns the access sequence for a new record. Callers hold the store
// mutex.
func (ix *index) nextSeq() uint64 {
	ix.seq++
	return ix.seq
}

func encodeRecord(m objectMeta) ([]byte, error) {
	sha1, err := hex.DecodeString(m.SHA1)
	if err != nil || len(sha1) != 20 {
		return nil, fmt.Errorf("invalid sha1 %q", m.SHA1)
	}
	buf := make([]byte, recordLen)
	binary.BigEndian.PutUint64(buf[0:8], uint64(m.Size))
	binary.BigEndian.PutUint64(buf[8:16], m.At)
	copy(buf[16:], sha1)
	return buf, nil
}

func decodeRecord(key, v []byte) (objectMeta, error) {
	if len(key) != 32 || len(v) != recordLen {
		return objectMeta{}, fmt.Errorf("corrupt index record (key %d bytes, value %d bytes)", len(key), len(v))
	}
	return objectMeta{
		SHA256: hex.EncodeToString(key),
		SHA1:   hex.EncodeToString(v[16:36]),
		Size:   int64(binary.BigEndian.Uint64(v[0:8])),
		At:     binary.BigEndian.Uint64(v[8:16]),
	}, nil
}

func rawKey(hexDigest string) ([]byte, error) {
	b, err := hex.DecodeString(hexDigest)
	if err != nil {
		return nil, fmt.Errorf("invalid digest %q", hexDigest)
	}
	return b, nil
}

// get looks a SHA-256 record up without touching recency.
func (ix *index) get(sha256 string) (objectMeta, bool, error) {
	key, err := rawKey(sha256)
	if err != nil {
		return objectMeta{}, false, err
	}
	var (
		meta  objectMeta
		found bool
	)
	err = ix.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(objectsBucket).Get(key)
		if v == nil {
			return nil
		}
		var err error
		meta, err = decodeRecord(key, v)
		found = err == nil
		return err
	})
	return meta, found, err
}

// lookupSHA1 resolves a SHA-1 alias to its SHA-256 primary key.
func (ix *index) lookupSHA1(sha1 string) (string, bool, error) {
	key, err := rawKey(sha1)
	if err != nil {
		return "", false, err
	}
	var found string
	err = ix.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(aliasesBucket).Get(key); v != nil {
			found = hex.EncodeToString(v)
		}
		return nil
	})
	return found, found != "", err
}

// touch promotes an existing SHA-256 record and returns its metadata. A false
// result means the record is unknown and the caller may adopt it.
func (ix *index) touch(sha256 string) (objectMeta, bool, error) {
	key, err := rawKey(sha256)
	if err != nil {
		return objectMeta{}, false, err
	}
	var (
		meta  objectMeta
		found bool
	)
	err = ix.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		v := b.Get(key)
		if v == nil {
			return nil
		}
		m, err := decodeRecord(key, v)
		if err != nil {
			return err
		}
		m.At = ix.nextSeq()
		encoded, err := encodeRecord(m)
		if err != nil {
			return err
		}
		meta, found = m, true
		return b.Put(key, encoded)
	})
	return meta, found, err
}

// put inserts or refreshes a record, including its SHA-1 alias, and returns the
// sequence it was stamped with.
func (ix *index) put(m objectMeta) (objectMeta, error) {
	key, err := rawKey(m.SHA256)
	if err != nil {
		return m, err
	}
	alias, err := rawKey(m.SHA1)
	if err != nil {
		return m, err
	}
	m.At = ix.nextSeq()
	encoded, err := encodeRecord(m)
	if err != nil {
		return m, err
	}
	err = ix.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(objectsBucket).Put(key, encoded); err != nil {
			return err
		}
		if err := tx.Bucket(aliasesBucket).Put(alias, key); err != nil {
			return err
		}
		// A re-PUT of a digest whose delete is still queued must cancel that
		// delete in the same transaction, or the next reconcile removes the body
		// that is now indexed and re-served.
		return tx.Bucket(pendingBucket).Delete(key)
	})
	return m, err
}

// remove deletes a record and its alias.
func (ix *index) remove(m objectMeta) error {
	key, err := rawKey(m.SHA256)
	if err != nil {
		return err
	}
	alias, err := rawKey(m.SHA1)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(objectsBucket).Delete(key); err != nil {
			return err
		}
		return tx.Bucket(aliasesBucket).Delete(alias)
	})
}

// removeAndQueue removes the record and durably records the body as pending
// deletion, so a failed object-store delete is retried instead of leaking
// capacity accounting.
func (ix *index) removeAndQueue(m objectMeta) error {
	key, err := rawKey(m.SHA256)
	if err != nil {
		return err
	}
	alias, err := rawKey(m.SHA1)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(objectsBucket).Delete(key); err != nil {
			return err
		}
		if err := tx.Bucket(aliasesBucket).Delete(alias); err != nil {
			return err
		}
		return tx.Bucket(pendingBucket).Put(key, alias)
	})
}

func (ix *index) clearPending(sha256 string) error {
	key, err := rawKey(sha256)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(pendingBucket).Delete(key)
	})
}

func (ix *index) pendingDeletes() ([]objectMeta, error) {
	var out []objectMeta
	err := ix.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(pendingBucket).ForEach(func(k, v []byte) error {
			out = append(out, objectMeta{
				SHA256: hex.EncodeToString(k),
				SHA1:   hex.EncodeToString(v),
			})
			return nil
		})
	})
	return out, err
}

// pruneDanglingAliases deletes alias rows whose primary object record is gone and
// reports how many it removed. Normal writes keep the two buckets in step, so
// this only repairs a damaged index (including pointers left behind for a body
// that vanished). The whole pass is one transaction: an error leaves it intact.
func (ix *index) pruneDanglingAliases() (int, error) {
	var dangling [][]byte
	removed := 0
	err := ix.db.Update(func(tx *bolt.Tx) error {
		aliases := tx.Bucket(aliasesBucket)
		objects := tx.Bucket(objectsBucket)
		if err := aliases.ForEach(func(k, v []byte) error {
			if objects.Get(v) == nil {
				dangling = append(dangling, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range dangling {
			if err := aliases.Delete(key); err != nil {
				return err
			}
		}
		removed = len(dangling)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// records returns every record ordered oldest access first, which is the order
// the in-memory LRU is rebuilt in.
func (ix *index) records() ([]objectMeta, error) {
	var out []objectMeta
	err := ix.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(objectsBucket).ForEach(func(k, v []byte) error {
			m, err := decodeRecord(k, v)
			if err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].SHA256 < out[j].SHA256
	})
	return out, nil
}
