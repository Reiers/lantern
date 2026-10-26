package store

// Auxiliary key namespace.
//
// Small derived indexes that must live and die with the header store (the
// eth tx index, chain/ethtxindex) persist here under their own prefix so
// they share the header store's BadgerDB (one data directory, dropped
// together on an auto-stale-reset) without being able to collide with
// header / canonical-pointer keys.

import (
	"errors"
	"fmt"

	"github.com/dgraph-io/badger/v4"
)

const prefixAux = "aux:"

// AuxPair is one key/value write in the auxiliary namespace.
type AuxPair struct {
	Key   []byte
	Value []byte
}

func auxKey(k []byte) []byte {
	out := make([]byte, 0, len(prefixAux)+len(k))
	out = append(out, prefixAux...)
	return append(out, k...)
}

// AuxGet reads key from the auxiliary namespace. ok=false when absent.
func (s *Store) AuxGet(key []byte) ([]byte, bool, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(auxKey(key))
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// AuxWrite applies deletes then puts in one transaction. If Badger reports
// the batch too big, it commits what it has and continues in a fresh
// transaction (callers keep batches small, so in practice every write is
// atomic; oversized prune batches are idempotent and safe to split).
func (s *Store) AuxWrite(puts []AuxPair, dels [][]byte) error {
	if len(puts) == 0 && len(dels) == 0 {
		return nil
	}
	txn := s.db.NewTransaction(true)
	defer func() { txn.Discard() }()
	apply := func(fn func() error) error {
		err := fn()
		if errors.Is(err, badger.ErrTxnTooBig) {
			if cerr := txn.Commit(); cerr != nil {
				return cerr
			}
			txn = s.db.NewTransaction(true)
			err = fn()
		}
		return err
	}
	for _, k := range dels {
		k := auxKey(k)
		if err := apply(func() error { return txn.Delete(k) }); err != nil {
			return fmt.Errorf("aux delete: %w", err)
		}
	}
	for _, p := range puts {
		k, v := auxKey(p.Key), p.Value
		if err := apply(func() error { return txn.Set(k, v) }); err != nil {
			return fmt.Errorf("aux set: %w", err)
		}
	}
	return txn.Commit()
}

// AuxIterate calls fn for every auxiliary key starting with prefix, in key
// order. Keys passed to fn have the namespace stripped; k and v are only
// valid for the duration of the call. Returning an error stops iteration.
func (s *Store) AuxIterate(prefix []byte, fn func(k, v []byte) error) error {
	full := auxKey(prefix)
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = full
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(full); it.ValidForPrefix(full); it.Next() {
			item := it.Item()
			k := item.Key()[len(prefixAux):]
			if err := item.Value(func(v []byte) error { return fn(k, v) }); err != nil {
				return err
			}
		}
		return nil
	})
}
