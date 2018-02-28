package etcdv3

import (
	"context"

	"github.com/YuleiXiao/kvstore"
	"github.com/YuleiXiao/kvstore/store"
	etcd "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	mvccpb "github.com/coreos/etcd/mvcc/mvccpb"
)

// Register registers etcd to kvstore
func Register() {
	kvstore.AddStore(store.ETCDV3, New)
}

// Etcd is the receiver type for the
// Store interface
type Etcd struct {
	client *etcd.Client
}

type etcdLock struct {
	mu *concurrency.Mutex
}

// New creates a new Etcd client given a list
// of endpoints and an optional tls config
func New(addrs []string, options *store.Config) (store.Store, error) {
	cfg := &etcd.Config{
		Endpoints: addrs,
	}

	// Set options
	if options != nil {
		if options.TLS != nil {
			cfg.TLS = options.TLS
		}
		if options.ConnectionTimeout != 0 {
			cfg.DialTimeout = options.ConnectionTimeout
		}
		if options.Username != "" {
			cfg.Username = options.Username
			cfg.Password = options.Password
		}
	}

	c, err := etcd.New(*cfg)
	if err != nil {
		return nil, err
	}

	s := &Etcd{
		client: c,
	}

	return s, nil
}

// Get the value at "key", returns the last modified
// index to use in conjunction to Atomic calls
func (s *Etcd) Get(ctx context.Context, key string) (pair *store.KVPair, err error) {
	pairs, err := s.get(ctx, key, false)
	if err != nil {
		return nil, err
	}

	return pairs[0], nil
}

func (s *Etcd) get(ctx context.Context, key string, prefix bool) (pairs []*store.KVPair, err error) {
	var resp *etcd.GetResponse
	var opts []etcd.OpOption
	if prefix {
		opts = []etcd.OpOption{etcd.WithPrefix()}
	}

	resp, err = s.client.Get(ctx, store.Normalize(key), opts...)
	if err != nil {
		return nil, err
	}

	if resp.Count == 0 {
		return nil, store.ErrKeyNotFound
	}

	pairs = []*store.KVPair{}
	for _, kv := range resp.Kvs {
		pairs = append(pairs, &store.KVPair{
			Key:     string(kv.Key),
			Value:   string(kv.Value),
			Index:   uint64(kv.ModRevision),
			Version: uint64(kv.Version),
			Lease:   uint64(kv.Lease),
		})
	}

	return pairs, nil
}

// Put a value at "key"
func (s *Etcd) Put(ctx context.Context, key, value string, opts *store.WriteOptions) error {
	key = store.Normalize(key)
	if opts != nil {
		resp, err := s.client.Grant(ctx, int64(opts.TTL.Seconds()))
		if err != nil {
			return err
		}
		_, err = s.client.Put(ctx, key, value, etcd.WithLease(resp.ID))
		return err
	}

	_, err := s.client.Put(ctx, key, value)
	return err
}

// Update is an alias for Put with key exist
func (s *Etcd) Update(ctx context.Context, key, value string, opts *store.WriteOptions) error {
	key = store.Normalize(key)

	req := etcd.OpPut(key, value)
	if opts != nil {
		leaseResp, err := s.client.Grant(ctx, int64(opts.TTL.Seconds()))
		if err != nil {
			return err
		}

		req = etcd.OpPut(key, value, etcd.WithLease(leaseResp.ID))
	}

	txn := s.client.Txn(ctx)
	resp, err := txn.If(etcd.Compare(etcd.CreateRevision(key), ">", 0)).Then(req).Commit()
	if err != nil {
		return err
	}

	if !resp.Succeeded {
		return store.ErrKeyNotFound
	}

	return nil
}

// Create is an alias for Put with key not exist
func (s *Etcd) Create(ctx context.Context, key, value string, opts *store.WriteOptions) error {
	key = store.Normalize(key)

	req := etcd.OpPut(key, value)
	if opts != nil {
		leaseResp, err := s.client.Grant(ctx, int64(opts.TTL.Seconds()))
		if err != nil {
			return err
		}

		req = etcd.OpPut(key, value, etcd.WithLease(leaseResp.ID))
	}

	txn := s.client.Txn(ctx)
	resp, err := txn.If(etcd.Compare(etcd.CreateRevision(key), "=", 0)).Then(req).Commit()
	if err != nil {
		return err
	}

	if !resp.Succeeded {
		return store.ErrKeyExists
	}

	return nil
}

// Delete a value at "key"
func (s *Etcd) Delete(ctx context.Context, key string) error {
	_, err := s.client.Delete(ctx, store.Normalize(key))
	return err
}

// Exists checks if the key exists inside the store
func (s *Etcd) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Get(ctx, key)
	if err != nil {
		if err == store.ErrKeyNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Watch for changes on a "key"
// It returns a channel that will receive changes or pass
// on errors. Upon creation, the current value will first
// be sent to the channel. Providing a non-nil stopCh can
// be used to stop watching.
func (s *Etcd) Watch(ctx context.Context, key string, opt *store.WatchOptions) (<-chan *store.WatchResponse, error) {
	return s.watch(ctx, key, false, opt)
}

// WatchTree watches for changes on a "directory"
// It returns a channel that will receive changes or pass
// on errors. Upon creating a watch, the current childs values
// will be sent to the channel. Providing a non-nil stopCh can
// be used to stop watching.
func (s *Etcd) WatchTree(ctx context.Context, directory string, opt *store.WatchOptions) (<-chan *store.WatchResponse, error) {
	return s.watch(ctx, directory, true, opt)
}

func (s *Etcd) watch(ctx context.Context, key string, prefix bool, opt *store.WatchOptions) (<-chan *store.WatchResponse, error) {
	var watchChan etcd.WatchChan
	opts := []etcd.OpOption{etcd.WithPrevKV()}
	if prefix {
		opts = append(opts, etcd.WithPrefix())
	}
	if opt != nil {
		opts = append(opts, etcd.WithRev(int64(opt.Index)))
	}

	watcher := etcd.NewWatcher(s.client)
	watchChan = watcher.Watch(ctx, store.Normalize(key), opts...)

	// resp is sending back events to the caller
	resp := make(chan *store.WatchResponse)
	go func() {
		defer func() {
			close(resp)
		}()
		defer func() {
			watcher.Close()
		}()

		for {
			select {
			case ch, ok := <-watchChan:
				for _, e := range ch.Events {
					resp <- s.makeWatchResponse(e, nil)
				}

				if !ok {
					resp <- s.makeWatchResponse(nil, store.ErrWatchFail)
					return
				}
			}
		}
	}()

	return resp, nil
}

func (s *Etcd) makeWatchResponse(event *etcd.Event, err error) *store.WatchResponse {
	if err != nil {
		return &store.WatchResponse{Error: err}
	}

	var action string
	switch event.Type {
	case mvccpb.PUT:
		action = store.ActionPut
	case mvccpb.DELETE:
		action = store.ActionDelete
	}

	var preNode *store.KVPair
	if event.PrevKv != nil {
		preNode = &store.KVPair{
			Key:     string(event.PrevKv.Key),
			Value:   string(event.PrevKv.Value),
			Index:   uint64(event.PrevKv.ModRevision),
			Version: uint64(event.PrevKv.Version),
			Lease:   uint64(event.PrevKv.Lease),
		}
	}

	return &store.WatchResponse{
		Action:  action,
		PreNode: preNode,
		Node: &store.KVPair{
			Key:     string(event.Kv.Key),
			Value:   string(event.Kv.Value),
			Index:   uint64(event.Kv.ModRevision),
			Version: uint64(event.PrevKv.Version),
			Lease:   uint64(event.PrevKv.Lease),
		},
	}
}

// AtomicPut puts a value at "key" if the key has not been
// modified in the meantime, throws an error if this is the case
func (s *Etcd) AtomicPut(ctx context.Context, key, value string, previous *store.KVPair, opts *store.WriteOptions) error {
	key = store.Normalize(key)

	req := etcd.OpPut(key, value)
	if opts != nil {
		leaseResp, err := s.client.Grant(ctx, int64(opts.TTL.Seconds()))
		if err != nil {
			return err
		}

		req = etcd.OpPut(key, value, etcd.WithLease(leaseResp.ID))
	}

	cmp := []etcd.Cmp{}
	if previous == nil {
		cmp = append(cmp, etcd.Compare(etcd.CreateRevision(key), "=", 0))
	} else {
		cmp = append(cmp, etcd.Compare(etcd.Value(key), "=", previous.Value))
		if previous.Index != 0 {
			cmp = append(cmp, etcd.Compare(etcd.ModRevision(key), "=", int64(previous.Index)))
		}
	}

	txn := s.client.Txn(ctx)
	resp, err := txn.If(cmp...).Then(req).Commit()
	if err != nil {
		return err
	}

	if resp.Succeeded {
		return nil
	}

	if previous == nil {
		return store.ErrKeyExists
	}
	return store.ErrKeyModified
}

// AtomicDelete deletes a value at "key" if the key
// has not been modified in the meantime, throws an
// error if this is the case
func (s *Etcd) AtomicDelete(ctx context.Context, key string, previous *store.KVPair) error {
	key = store.Normalize(key)

	if previous == nil {
		return store.ErrPreviousNotSpecified
	}

	cmp := []etcd.Cmp{etcd.Compare(etcd.Value(key), "=", previous.Value)}
	if previous.Index != 0 {
		cmp = append(cmp, etcd.Compare(etcd.ModRevision(key), "=", int64(previous.Index)))
	}

	txn := s.client.Txn(ctx)
	resp, err := txn.If(cmp...).Then(
		etcd.OpDelete(key),
	).Commit()

	if err != nil {
		return err
	}

	if !resp.Succeeded {
		return store.ErrKeyModified
	}

	return nil
}

// List child nodes of a given directory
func (s *Etcd) List(ctx context.Context, directory string) ([]*store.KVPair, error) {
	pairs, err := s.get(ctx, store.Normalize(directory), true)
	if err != nil {
		return nil, err
	}

	return pairs, nil
}

// DeleteTree deletes a range of keys under a given directory
func (s *Etcd) DeleteTree(ctx context.Context, directory string) error {
	_, err := s.client.Delete(ctx, store.Normalize(directory), etcd.WithPrefix())
	return err
}

// NewLock creates a lock for a given key.
// The returned Locker is not held and must be acquired
// with `.Lock`. The Value is optional.
func (s *Etcd) NewLock(key string, opt *store.LockOptions) store.Locker {
	var session *concurrency.Session
	if opt != nil {
		session, _ = concurrency.NewSession(s.client, concurrency.WithTTL(int(opt.TTL.Seconds())))
	} else {
		session, _ = concurrency.NewSession(s.client)
	}
	return concurrency.NewMutex(session, key)
}

// Compact compacts etcd KV history before the given rev.
func (s *Etcd) Compact(ctx context.Context, rev uint64, wait bool) error {
	if wait {
		_, err := s.client.Compact(ctx, int64(rev), etcd.WithCompactPhysical())
		return err
	}
	_, err := s.client.Compact(ctx, int64(rev))
	return err
}

// NewTxn creates a transaction Txn.
func (s *Etcd) NewTxn(ctx context.Context) (store.Txn, error) {
	return &txn{
		ctx:    ctx,
		client: s.client,
	}, nil
}

// Close closes the client connection
func (s *Etcd) Close() {
	s.client.Close()
	return
}
