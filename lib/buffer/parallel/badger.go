// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package parallel

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jeffail/benthos/lib/types"
	"github.com/dgraph-io/badger"
)

//------------------------------------------------------------------------------

var benthosSeqKey = []byte("benthos_msg_seq")

//------------------------------------------------------------------------------

// BadgerConfig contains configuration fields for a badger based buffer.
type BadgerConfig struct {
	Directory    string `json:"directory" yaml:"directory"`
	SyncWrites   bool   `json:"sync_writes" yaml:"sync_writes"`
	GCIntervalMS int    `json:"gc_interval_ms" yaml:"gc_interval_ms"`
}

// NewBadgerConfig creates a BadgerConfig with default values.
func NewBadgerConfig() BadgerConfig {
	return BadgerConfig{
		Directory:    "",
		SyncWrites:   true,
		GCIntervalMS: 1000,
	}
}

//------------------------------------------------------------------------------

// Badger is a parallel buffer implementation that allows multiple parallel
// consumers to read and purge messages from a Badger embedded key/value store,
// where messages are persisted to disk.
type Badger struct {
	messageKeys [][]byte

	pendingCtr int64

	gcInterval time.Duration

	db   *badger.DB
	seq  *badger.Sequence
	cond *sync.Cond
}

// NewBadger creates a Badger based parallel buffer.
func NewBadger(conf BadgerConfig) (*Badger, error) {
	opts := badger.DefaultOptions
	opts.Dir = conf.Directory
	opts.ValueDir = conf.Directory
	opts.SyncWrites = conf.SyncWrites

	b := &Badger{
		gcInterval: time.Millisecond * time.Duration(conf.GCIntervalMS),
		cond:       sync.NewCond(&sync.Mutex{}),
	}

	var err error
	if b.db, err = badger.Open(opts); err != nil {
		return nil, err
	}
	if b.seq, err = b.db.GetSequence(benthosSeqKey, 1000); err != nil {
		return nil, err
	}

	if err = b.db.View(func(txn *badger.Txn) error {
		iterOpts := badger.DefaultIteratorOptions
		iterOpts.PrefetchValues = false
		iter := txn.NewIterator(iterOpts)
		defer iter.Close()
		for iter.Rewind(); iter.Valid(); iter.Next() {
			key := iter.Item().Key()
			if bytes.Equal(benthosSeqKey, key) {
				continue
			}
			keyBytes := make([]byte, len(key))
			copy(keyBytes, key)
			b.messageKeys = append(b.messageKeys, keyBytes)
			atomic.AddInt64(&b.pendingCtr, 1)
		}
		return nil
	}); err != nil {
		b.db.Close()
		return nil, err
	}

	go b.gcLoop()

	return b, nil
}

//------------------------------------------------------------------------------

func (b *Badger) gcLoop() {
	for {
		<-time.After(b.gcInterval)

		b.cond.L.Lock()
		if b.db == nil {
			b.cond.L.Unlock()
			return
		}
		b.db.RunValueLogGC(0.5)
		b.cond.L.Unlock()
	}
}

//------------------------------------------------------------------------------

// NextMessage reads the next oldest message, the message is preserved until the
// returned AckFunc is called.
func (b *Badger) NextMessage() (types.Message, AckFunc, error) {
	b.cond.L.Lock()
	for len(b.messageKeys) == 0 && b.db != nil {
		b.cond.Wait()
	}

	if b.db == nil {
		b.cond.L.Unlock()
		return nil, nil, types.ErrTypeClosed
	}

	key := b.messageKeys[0]

	b.messageKeys[0] = nil
	b.messageKeys = b.messageKeys[1:]

	b.cond.L.Unlock()

	var msg types.Message

	if err := b.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		borrowMsg, err := types.FromBytes(val)
		if err != nil {
			return err
		}
		msg = borrowMsg.DeepCopy()
		return nil
	}); err != nil {
		b.cond.L.Lock()
		b.messageKeys = append([][]byte{key}, b.messageKeys...)
		b.cond.Broadcast()
		b.cond.L.Unlock()
		return nil, nil, err
	}

	return msg, func(ack bool) (int, error) {
		b.cond.L.Lock()
		if b.db == nil {
			b.cond.L.Unlock()
			return 0, types.ErrTypeClosed
		}
		var err error
		if ack {
			if err = b.db.Update(func(txn *badger.Txn) error {
				return txn.Delete(key)
			}); err == nil {
				atomic.AddInt64(&b.pendingCtr, -1)
			}
		} else {
			b.messageKeys = append([][]byte{key}, b.messageKeys...)
		}
		_, backlog := b.db.Size()

		b.cond.Broadcast()
		b.cond.L.Unlock()

		return int(backlog), err
	}, nil
}

// PushMessage adds a new message to the stack. Returns the backlog in bytes.
func (b *Badger) PushMessage(msg types.Message) (int, error) {
	b.cond.L.Lock()

	if b.db == nil {
		b.cond.L.Unlock()
		return 0, types.ErrTypeClosed
	}

	keyNum, err := b.seq.Next()
	if err != nil {
		b.cond.L.Unlock()
		return 0, err
	}

	key := []byte(fmt.Sprintf("%020d", keyNum))

	if err = b.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, msg.Bytes())
	}); err != nil {
		b.cond.L.Unlock()
		return 0, err
	}

	b.messageKeys = append(b.messageKeys, key)
	atomic.AddInt64(&b.pendingCtr, 1)

	_, backlog := b.db.Size()

	b.cond.Broadcast()
	b.cond.L.Unlock()

	return int(backlog), nil
}

func (b *Badger) cleanUpBadger() (err error) {
	if b.seq != nil {
		b.seq = nil
	}
	if b.db != nil {
		err = b.db.Close()
		b.db = nil
	}
	return
}

// CloseOnceEmpty closes the Buffer once the buffer has been emptied, this is a
// way for a writer to signal to a reader that it is finished writing messages,
// and therefore the reader can close once it is caught up. This call blocks
// until the close is completed.
func (b *Badger) CloseOnceEmpty() {
	b.cond.L.Lock()
	for atomic.LoadInt64(&b.pendingCtr) > 0 && b.db != nil {
		b.cond.Wait()
	}
	b.cleanUpBadger()
	b.cond.Broadcast()
	b.cond.L.Unlock()
}

// Close closes the Buffer so that blocked readers or writers become
// unblocked.
func (b *Badger) Close() {
	b.cond.L.Lock()
	b.cleanUpBadger()
	b.cond.Broadcast()
	b.cond.L.Unlock()
}

//------------------------------------------------------------------------------
