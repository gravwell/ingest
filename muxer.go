/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package ingest

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravwell/ingest/v3/entry"
	"github.com/gravwell/ingest/v3/log"
)

var (
	ErrAllConnsDown          = errors.New("All connections down")
	ErrNotRunning            = errors.New("Not running")
	ErrNotReady              = errors.New("Not ready to start")
	ErrTagNotFound           = errors.New("Tag not found")
	ErrTagMapInvalid         = errors.New("Tag map invalid")
	ErrNoTargets             = errors.New("No connections specified")
	ErrConnectionTimeout     = errors.New("Connection timeout")
	ErrSyncTimeout           = errors.New("Sync timeout")
	ErrEmptyAuth             = errors.New("Ingest key is empty")
	ErrEmergencyListOverflow = errors.New("Emergency list overflow")
	ErrTimeout               = errors.New("Timed out waiting for ingesters")
	ErrWriteTimeout          = errors.New("Timed out waiting to write entry")

	errNotImp = errors.New("Not implemented yet")
)

const (
	empty   muxState = 0
	running muxState = 1
	closed  muxState = 2

	defaultChannelSize   int           = 64
	defaultRetryTime     time.Duration = 10 * time.Second
	recycleTimeout       time.Duration = time.Second
	maxEmergencyListSize int           = 256
	unknownAddr          string        = `unknown`
	waitTickerDur        time.Duration = 50 * time.Millisecond
)

type muxState int

type Target struct {
	Address string
	Secret  string
}

type TargetError struct {
	Address string
	Error   error
}

type IngestMuxer struct {
	//connHot, and connDead have atomic operations
	//its important that these are aligned on 8 byte boundaries
	//or it will panic on 32bit architectures
	connHot         int32 //how many connections are functioning
	connDead        int32 //how many connections are dead
	mtx             *sync.RWMutex
	sig             *sync.Cond
	igst            []*IngestConnection
	tagTranslators  []*tagTrans
	dests           []Target
	errDest         []TargetError
	tags            []string
	tagMap          map[string]entry.EntryTag
	pubKey          string
	privKey         string
	verifyCert      bool
	eChan           chan *entry.Entry
	bChan           chan []*entry.Entry
	eq              *emergencyQueue
	dieChan         chan bool
	upChan          chan bool
	errChan         chan error
	wg              *sync.WaitGroup
	state           muxState
	logLevel        gll
	lgr             Logger
	cacheEnabled    bool
	cache           *IngestCache
	cacheWg         *sync.WaitGroup
	cacheFileBacked bool
	cacheRunning    bool
	cacheError      error
	cacheSignal     chan bool
	name            string
	version         string
	uuid            string
	rateParent      *parent
}

type UniformMuxerConfig struct {
	Destinations    []string
	Tags            []string
	Auth            string
	PublicKey       string
	PrivateKey      string
	VerifyCert      bool
	ChannelSize     int
	EnableCache     bool
	CacheConfig     IngestCacheConfig
	LogLevel        string
	Logger          Logger
	IngesterName    string
	IngesterVersion string
	IngesterUUID    string
	RateLimitBps    int64
}

type MuxerConfig struct {
	Destinations    []Target
	Tags            []string
	PublicKey       string
	PrivateKey      string
	VerifyCert      bool
	ChannelSize     int
	EnableCache     bool
	CacheConfig     IngestCacheConfig
	LogLevel        string
	Logger          Logger
	IngesterName    string
	IngesterVersion string
	IngesterUUID    string
	RateLimitBps    int64
}

func NewUniformMuxer(c UniformMuxerConfig) (*IngestMuxer, error) {
	return newUniformIngestMuxerEx(c)
}

func NewMuxer(c MuxerConfig) (*IngestMuxer, error) {
	return newIngestMuxer(c)
}

// NewIngestMuxer creates a new muxer that will automatically distribute entries amongst the clients
func NewUniformIngestMuxer(dests, tags []string, authString, pubKey, privKey, remoteKey string) (*IngestMuxer, error) {
	return NewUniformIngestMuxerExt(dests, tags, authString, pubKey, privKey, remoteKey, defaultChannelSize)
}

func NewUniformIngestMuxerExt(dests, tags []string, authString, pubKey, privKey, remoteKey string, chanSize int) (*IngestMuxer, error) {
	c := UniformMuxerConfig{
		Destinations: dests,
		Tags:         tags,
		Auth:         authString,
		PublicKey:    pubKey,
		PrivateKey:   privKey,
		ChannelSize:  chanSize,
	}
	return newUniformIngestMuxerEx(c)
}

func newUniformIngestMuxerEx(c UniformMuxerConfig) (*IngestMuxer, error) {
	if len(c.Auth) == 0 {
		return nil, ErrEmptyAuth
	}
	destinations := make([]Target, len(c.Destinations))
	for i := range c.Destinations {
		destinations[i].Address = c.Destinations[i]
		destinations[i].Secret = c.Auth
	}
	if len(destinations) == 0 {
		return nil, ErrNoTargets
	}
	cfg := MuxerConfig{
		Destinations:    destinations,
		Tags:            c.Tags,
		PublicKey:       c.PublicKey,
		PrivateKey:      c.PrivateKey,
		VerifyCert:      c.VerifyCert,
		ChannelSize:     c.ChannelSize,
		EnableCache:     c.EnableCache,
		CacheConfig:     c.CacheConfig,
		LogLevel:        c.LogLevel,
		IngesterName:    c.IngesterName,
		IngesterVersion: c.IngesterVersion,
		IngesterUUID:    c.IngesterUUID,
		RateLimitBps:    c.RateLimitBps,
		Logger:          c.Logger,
	}
	return newIngestMuxer(cfg)
}

func NewIngestMuxer(dests []Target, tags []string, pubKey, privKey string) (*IngestMuxer, error) {
	return NewIngestMuxerExt(dests, tags, pubKey, privKey, defaultChannelSize)
}

func NewIngestMuxerExt(dests []Target, tags []string, pubKey, privKey string, chanSize int) (*IngestMuxer, error) {
	c := MuxerConfig{
		Destinations: dests,
		Tags:         tags,
		PublicKey:    pubKey,
		PrivateKey:   privKey,
		ChannelSize:  chanSize,
	}
	return newIngestMuxer(c)
}

func newIngestMuxer(c MuxerConfig) (*IngestMuxer, error) {
	localTags := make([]string, 0, len(c.Tags))
	for i := range c.Tags {
		localTags = append(localTags, c.Tags[i])
	}
	if c.Logger == nil {
		c.Logger = log.NewDiscardLogger()
	}

	//if the cache is enabled, attempt to fire it up
	var cache *IngestCache
	var cacheSig chan bool
	var err error
	if c.EnableCache {
		cache, err = NewIngestCache(c.CacheConfig)
		if err != nil {
			return nil, err
		}
		cacheSig = make(chan bool, 1)

		// If there were stored entries, re-initialize localTags and the tagMap
		if cache.Count() > 0 {
			ctags, err := cache.GetTagList()
			if err != nil {
				return nil, err
			}
			if len(ctags) > 0 {
				// First, check if there are cached tags which are NOT in our configured set
				var uniques []string
			uniqueLoop:
				for _, ct := range ctags {
					for _, lt := range localTags {
						if ct == lt {
							continue uniqueLoop
						}
					}
					uniques = append(uniques, ct)
				}
				if len(uniques) > 0 {
					c.Logger.Warn("The cache file contains entries. To ensure ingestion under the correct tags, the ingester will negotiate the following tags even if the config file does not currently require them: %v", uniques)
				}

				// Now, append any new configured tags to the end of the cached tags and use that as our localTags
			tagLoop:
				for _, lt := range localTags {
					for _, ct := range ctags {
						if lt == ct {
							// the tag was already in the set, skip
							continue tagLoop
						}
					}
					ctags = append(ctags, lt)
				}
				localTags = ctags
			}
		}
		// Now update the stored tags list no matter what
		if err := cache.UpdateStoredTagList(localTags); err != nil {
			return nil, err
		}
	}

	//generate our tag map, the tag map is used only for quick tag lookup/translation by routines
	tagMap := make(map[string]entry.EntryTag, len(localTags))
	for i, v := range localTags {
		tagMap[v] = entry.EntryTag(i)
	}

	if c.ChannelSize <= 0 {
		c.ChannelSize = defaultChannelSize
	}

	var p *parent
	if c.RateLimitBps > 0 {
		p = newParent(c.RateLimitBps, 0)
	}
	return &IngestMuxer{
		dests:           c.Destinations,
		tags:            localTags,
		tagMap:          tagMap,
		pubKey:          c.PublicKey,
		privKey:         c.PrivateKey,
		verifyCert:      c.VerifyCert,
		mtx:             &sync.RWMutex{},
		wg:              &sync.WaitGroup{},
		state:           empty,
		lgr:             c.Logger,
		logLevel:        logLevel(c.LogLevel),
		eChan:           make(chan *entry.Entry, c.ChannelSize),
		bChan:           make(chan []*entry.Entry, c.ChannelSize),
		eq:              newEmergencyQueue(),
		dieChan:         make(chan bool, len(c.Destinations)),
		upChan:          make(chan bool, 1),
		errChan:         make(chan error, len(c.Destinations)),
		cache:           cache,
		cacheEnabled:    c.EnableCache,
		cacheWg:         &sync.WaitGroup{},
		cacheFileBacked: c.CacheConfig.FileBackingLocation != ``,
		cacheSignal:     cacheSig,
		name:            c.IngesterName,
		version:         c.IngesterVersion,
		uuid:            c.IngesterUUID,
		rateParent:      p,
	}, nil
}

//Start starts the connection process. This will return immediately, and does
//not mean that connections are ready. Callers should call WaitForHot immediately after
//to wait for the connections to be ready.
func (im *IngestMuxer) Start() error {
	im.mtx.Lock()
	defer im.mtx.Unlock()
	if im.state != empty || len(im.igst) != 0 {
		return ErrNotReady
	}
	//fire up the cache if its in use
	if im.cacheEnabled {
		im.cacheWg.Add(1)
		im.cacheRunning = true
		go im.cacheRoutine()
	}

	//fire up the ingest routines
	im.igst = make([]*IngestConnection, len(im.dests))
	im.tagTranslators = make([]*tagTrans, len(im.dests))
	im.wg.Add(len(im.dests))
	im.connDead = int32(len(im.dests))
	for i := 0; i < len(im.dests); i++ {
		go im.connRoutine(i)
	}
	im.state = running
	return nil
}

// Close the connection
func (im *IngestMuxer) Close() error {
	// Inform the world that we're done.
	im.Info("Ingester %v exiting\n", im.name)
	im.Sync(time.Second)

	var ok bool

	im.mtx.Lock()
	if im.state == closed {
		im.mtx.Unlock()
		return nil
	}
	im.state = closed

	//just close the channel, that will be a permanent signal for everything to close
	close(im.dieChan)

	//there is a chance that we are fully blocked with another async caller
	//writing to the channel, so we set the state to closed and check if we need to
	//discard some items from the channel
	if atomic.LoadInt32(&im.connHot) == 0 && !im.cacheRunning {
		//no connections are hot, and there is no cache
		//closing is GOING to pitch entries, so... it is what it is...
		//clear the channels
	consumer:
		for {
			select {
			case _, ok = <-im.eChan:
				if !ok {
					break consumer
				}
			case _, ok = <-im.bChan:
				if !ok {
					break consumer
				}
			default:
				break consumer
			}
		}
	}

	//we MUST unlock the mutex while we wait so that if a connection
	//goes into an errors state it can lock the mutex to adjust the errDest
	im.mtx.Unlock()

	//wait for everyone to quit
	im.wg.Wait()

	//if the cache is in use, signal for it to terminate and wait
	if im.cacheRunning && im.cacheSignal != nil {
		close(im.cacheSignal)
		im.cacheWg.Wait()
	}

	im.mtx.Lock()
	defer im.mtx.Unlock()

	//close the echan now that all the routines have closed
	close(im.eChan)
	close(im.bChan)

	//sync the cache and close it
	if im.cacheEnabled && im.cache != nil {
		if im.cacheFileBacked {
			// pull all outstanding items from each ingester connection and the channel
			// and shove them into the cache, then sync it
			for i := range im.igst {
				if im.igst[i] == nil {
					continue //skip nil ingesters, these SHOULDN'T be nil
				}
				ents := im.igst[i].outstandingEntries()
				for i := range ents {
					if ents[i] == nil {
						continue
					}
					im.cache.addEntry(ents[i])
				}
			}
			//clean out the entry channel too
			for e := range im.eChan {
				if e == nil {
					continue
				}
				im.cache.addEntry(e)
			}
			//clean out the entry block channel too
			for b := range im.bChan {
				if b == nil {
					continue
				}
				for _, e := range b {
					if e == nil {
						continue
					}
					im.cache.addEntry(e)
				}
			}

			// clear the emergency queue into cache
			for {
				ent, ents, ok := im.eq.pop()
				if !ok {
					break
				}
				if ent != nil {
					im.cache.addEntry(ent)
				}
				if len(ents) > 0 {
					for _, e := range ents {
						im.cache.addEntry(e)
					}
				}
			}

			//if we are file backed, sync the backing cache
			if err := im.cache.Sync(); err != nil {
				return err
			}
		}
		if err := im.cache.UpdateStoredTagList(im.tags); err != nil {
			return err
		}
		if err := im.cache.Close(); err != nil {
			return err
		}
	}

	//everyone is dead, clean up
	close(im.upChan)
	return nil
}

// LookupTag will reverse a tag id into a name, this operation is more expensive than a straight lookup
// Users that expect to translate a tag repeatedly should maintain their own tag map
func (im *IngestMuxer) LookupTag(tg entry.EntryTag) (name string, ok bool) {
	im.mtx.RLock()
	for k, v := range im.tagMap {
		if v == tg {
			name = k
			ok = true
			break
		}
	}
	im.mtx.RUnlock()
	return
}

// NegotiateTag will attempt to lookup a tag name in the negotiated set
// The the tag name has not already been negotiated, the muxer will contact
// each indexer and negotiate it.  This call can potentially block and fail
func (im *IngestMuxer) NegotiateTag(name string) (tg entry.EntryTag, err error) {
	if err = CheckTag(name); err != nil {
		return
	}

	im.mtx.Lock()
	defer im.mtx.Unlock()

	if tag, ok := im.tagMap[name]; ok {
		// tag already exists, just return it
		tg = tag
		return
	}

	// update the tag list and map
	im.tags = append(im.tags, name)
	for i, v := range im.tags {
		im.tagMap[v] = entry.EntryTag(i)
	}
	if im.cacheEnabled && im.cacheFileBacked {
		// Now update the stored tags list
		if err = im.cache.UpdateStoredTagList(im.tags); err != nil {
			return
		}
	}
	tg = im.tagMap[name]

	for k, v := range im.igst {
		if v != nil {
			remoteTag, err := v.NegotiateTag(name)
			if err != nil {
				// something went wrong, kill it and let it re-initialize
				v.Close()
				continue
			}
			if im.tagTranslators[k] != nil {
				err = im.tagTranslators[k].RegisterTag(tg, remoteTag)
				if err != nil {
					v.Close()
				}
			} else {
				v.Close()
			}
		}
	}
	return
}

func (im *IngestMuxer) Sync(to time.Duration) error {
	return im.SyncContext(context.Background(), to)
}

func (im *IngestMuxer) SyncContext(ctx context.Context, to time.Duration) error {
	if atomic.LoadInt32(&im.connHot) == 0 && !im.cacheRunning {
		return ErrAllConnsDown
	}
	ts := time.Now()
	im.mtx.Lock()
	for len(im.eChan) > 0 || len(im.bChan) > 0 {
		if err := ctx.Err(); err != nil {
			im.mtx.Unlock()
			return err
		}
		time.Sleep(10 * time.Millisecond)
		if im.connHot == 0 {
			im.mtx.Unlock()
			return ErrAllConnsDown
		}
		if time.Since(ts) > to {
			im.mtx.Unlock()
			return ErrTimeout
		}
	}

	var count int
	for _, v := range im.igst {
		if v != nil {
			if err := v.Sync(); err != nil {
				if err == ErrNotRunning {
					count++
				}
			}
		}
	}
	im.mtx.Unlock()
	if count == len(im.igst) {
		return ErrAllConnsDown
	}
	return nil
}

// WaitForHot waits until at least one connection goes into the hot state
// The timeout duration parameter is an optional timeout, if zero, it waits
// indefinitely
func (im *IngestMuxer) WaitForHot(to time.Duration) error {
	return im.WaitForHotContext(context.Background(), to)
}

func (im *IngestMuxer) WaitForHotContext(ctx context.Context, to time.Duration) error {
	if cnt, err := im.Hot(); err != nil {
		return err
	} else if cnt > 0 {
		return nil
	}

	//no connections are up, wait for them
	tckDur := waitTickerDur
	if to > 0 && to < tckDur {
		tckDur = to
	}
	tckr := time.NewTicker(tckDur)
	defer tckr.Stop()
	ts := time.Now()

	//wait for one of them to hit
mainLoop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-im.upChan:
			im.Info("Ingester %v has gone hot", im.name)
			break mainLoop
		case <-tckr.C:
			//check if connections are hot
			if cnt, err := im.Hot(); err != nil {
				return err
			} else if cnt > 0 {
				return nil //connection went hot
			}

			if to == 0 {
				continue // no timeout, wait forever
			} else if time.Since(ts) < to {
				//we haven't hit our timeout yet, just continue
				continue
			}
			//timeout, check state and force a return
			//if we have a hot, filebacked cache, then endpoints are go for ingest
			if im.cacheRunning && im.cacheError == nil && im.cacheFileBacked {
				return nil
			}
			return ErrConnectionTimeout
		case err := <-im.errChan:
			//lock the mutex and check if all our connections failed
			im.mtx.RLock()
			if len(im.errDest) == len(im.dests) {
				im.mtx.RUnlock()
				return errors.New("All connections failed " + err.Error())
			}
			im.mtx.RUnlock()
			continue
		}
	}
	return nil //someone came up
}

// Hot returns how many connections are functioning
func (im *IngestMuxer) Hot() (int, error) {
	im.mtx.RLock()
	defer im.mtx.RUnlock()
	if im.state != running {
		return -1, ErrNotRunning
	}
	return int(atomic.LoadInt32(&im.connHot)), nil
}

// unload cache will attempt to push out to the ingest connection
// the returned boolean indicates whether we were able to entirely unload the cache
// the cache MUST be stopped when we call this function
// we are potentially bypassing the channel and adding directly into it
func (im *IngestMuxer) unloadCache() (bool, error) {
	//attempt to pull all our entries from the cache and push them through the entry channel
	//this is used when a connection goes hot, we pull from our cache and drop them into channel
	//for the muxer to fire at indexers
	for {
		//pop a block and attempt to push into the ingest routine
		blk, err := im.cache.PopBlock()
		if err != nil {
			return false, err
		}
		if blk == nil {
			break //no more blocks
		}
		ents := blk.Entries()
		select {
		case im.bChan <- ents:
		case _, ok := <-im.cacheSignal:
			//push things back into the cache if we have zero connections or
			// the cacheSignal channel closed
			v := atomic.LoadInt32(&im.connHot)
			//if !ok || atomic.LoadInt32(&im.connHot) == 0 {
			if !ok || v == 0 {
				//push the block items back into the cache and bail
				im.cache.addBlock(ents)
				return false, nil //we need a transition
			}
		}
	}
	return true, nil
}

func (im *IngestMuxer) cacheRoutine() {
	defer im.cacheWg.Done()
	var cacheActive bool

	//when the cache is fired up, we ALWAYS start
	//that way we are guaranteed to be able to consume entries
	if err := im.cache.Start(im.eChan, im.bChan); err != nil {
		im.cacheError = err
		im.cacheRunning = false
		return
	}
	cacheActive = true

mainLoop:
	for {
		if _, ok := <-im.cacheSignal; !ok {
			break mainLoop
		}
		//we have been signaled about a start or stop
		if atomic.LoadInt32(&im.connHot) > 0 {
			if cacheActive == true {
				//a connection just went hot, stop the cache and
				//attempt to dump entries out to the connection
				cacheActive = false
				if err := im.cache.Stop(); err != nil {
					im.cacheError = err
					break mainLoop
				}
				//attempt to unload the cache
				emptied, err := im.unloadCache()
				if err != nil {
					im.cacheError = err
					break mainLoop
				}
				if !emptied {
					//the cache couldn't empty due to ingesters disconnecting
					//fire it back up and continue our loop
					cacheActive = true
					if err := im.cache.Start(im.eChan, im.bChan); err != nil {
						im.cacheError = err
						break mainLoop
					}
				}
			}
			//we were not active and another ingester came online, do nothing
		} else {
			//no hot connections
			if cacheActive == false {
				//we just transitioned into no active ingest links
				//and the cache is not active, get it fired up and rolling
				cacheActive = true
				if err := im.cache.Start(im.eChan, im.bChan); err != nil {
					im.cacheError = err
					break mainLoop
				}
			}
		}
	}

	//check if we need to stop the cache on our way out
	if cacheActive {
		if err := im.cache.Stop(); err != nil {
			im.cacheError = err
		}
		cacheActive = false
	}
	im.cacheRunning = false
}

//goHot is a convenience function used by routines when they become active
func (im *IngestMuxer) goHot() {
	atomic.AddInt32(&im.connDead, -1)
	//attempt a single on going hot, but don't block
	//increment the hot counter
	if atomic.AddInt32(&im.connHot, 1) == 1 {
		im.stopCache()
	}
	select {
	case im.upChan <- true:
	default:
	}
}

func (im *IngestMuxer) startCache() {
	if im.cacheRunning {
		//try to tell the cache about the need to fire back up
		//if we can't send the signal, then the cache routine is busy.
		//this is fine, because the cache routine will test the hot count
		//in its loop and do the right thing
		select {
		case im.cacheSignal <- true: //true means an ingester stopped
		default:
		}

	}
}

func (im *IngestMuxer) stopCache() {
	if im.cacheRunning {
		//try to tell the cache about the stoppage
		//if we can't send the signal, then the cache routine is busy
		//this is fine, because the cache routine will test the hot count
		//in its loop and do the right thing
		select {
		case im.cacheSignal <- false: //false means an ingester started
		default:
		}
	}
}

//goDead is a convenience function used by routines when they become dead
func (im *IngestMuxer) goDead() {
	//decrement the hot counter
	if atomic.AddInt32(&im.connHot, -1) == 0 {
		im.startCache()
	}
	atomic.AddInt32(&im.connDead, 1)
}

// Dead returns how many connections are currently dead
func (im *IngestMuxer) Dead() (int, error) {
	im.mtx.RLock()
	defer im.mtx.RUnlock()
	if im.state != running {
		return -1, ErrNotRunning
	}
	return int(im.connDead), nil
}

// Size returns the total number of specified connections, hot or dead
func (im *IngestMuxer) Size() (int, error) {
	im.mtx.RLock()
	defer im.mtx.RUnlock()
	if im.state != running {
		return -1, ErrNotRunning
	}
	return len(im.dests), nil
}

// GetTag pulls back an intermediary tag id
// the intermediary tag has NO RELATION to the backend servers tag mapping
// it is used to speed along tag mappings
func (im *IngestMuxer) GetTag(tag string) (tg entry.EntryTag, err error) {
	var ok bool
	im.mtx.RLock()
	if tg, ok = im.tagMap[tag]; !ok {
		err = ErrTagNotFound
	}
	im.mtx.RUnlock()
	return
}

// WriteEntry puts an entry into the queue to be sent out by the first available
// entry writer routine, if all routines are dead, THIS WILL BLOCK once the
// channel fills up.  We figure this is a natural "wait" mechanism
func (im *IngestMuxer) WriteEntry(e *entry.Entry) error {
	if e == nil {
		return nil
	}
	im.mtx.RLock()
	runok := im.state == running
	im.mtx.RUnlock()
	if !runok {
		return ErrNotRunning
	}
	im.eChan <- e
	return nil
}

// WriteEntryContext puts an entry into the queue to be sent out by the first available
// entry writer routine, if all routines are dead, THIS WILL BLOCK once the
// channel fills up.  We figure this is a natural "wait" mechanism
// if not using a context, use WriteEntry as it is faster due to the lack of a select
func (im *IngestMuxer) WriteEntryContext(ctx context.Context, e *entry.Entry) error {
	if e == nil {
		return nil
	}
	im.mtx.RLock()
	runok := im.state == running
	im.mtx.RUnlock()
	if !runok {
		return ErrNotRunning
	}
	select {
	case im.eChan <- e:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// WriteEntryAttempt attempts to put an entry into the queue to be sent out
// of the first available writer routine.  This write is opportunistic and contains
// a timeout.  It is therefor every expensive and shouldn't be used for normal writes
// The typical use case is via the gravwell_log calls
func (im *IngestMuxer) WriteEntryTimeout(e *entry.Entry, d time.Duration) (err error) {
	tmr := time.NewTimer(d)
	if e == nil {
		return
	}
	im.mtx.RLock()
	runok := im.state == running
	im.mtx.RUnlock()
	if !runok {
		return ErrNotRunning
	}
	select {
	case im.eChan <- e:
	case _ = <-tmr.C:
		err = ErrWriteTimeout
	}
	return
}

// WriteBatch puts a slice of entries into the queue to be sent out by the first
// available entry writer routine.  The entry writer routines will consume the
// entire slice, so extremely large slices will go to a single indexer.
func (im *IngestMuxer) WriteBatch(b []*entry.Entry) error {
	if len(b) == 0 {
		return nil
	}
	im.mtx.RLock()
	runok := im.state == running
	im.mtx.RUnlock()
	if !runok {
		return ErrNotRunning
	}
	im.bChan <- b
	return nil
}

// WriteBatchContext puts a slice of entries into the queue to be sent out by the first
// available entry writer routine.  The entry writer routines will consume the
// entire slice, so extremely large slices will go to a single indexer.
// if a cancellation context isn't needed, use WriteBatch
func (im *IngestMuxer) WriteBatchContext(ctx context.Context, b []*entry.Entry) error {
	if len(b) == 0 {
		return nil
	}
	im.mtx.RLock()
	runok := im.state == running
	im.mtx.RUnlock()
	if !runok {
		return ErrNotRunning
	}
	select {
	case im.bChan <- b:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Write puts together the arguments to create an entry and writes it
// to the queue to be sent out by the first available
// entry writer routine, if all routines are dead, THIS WILL BLOCK once the
// channel fills up.  We figure this is a natural "wait" mechanism
func (im *IngestMuxer) Write(tm entry.Timestamp, tag entry.EntryTag, data []byte) error {
	e := &entry.Entry{
		Data: data,
		TS:   tm,
		Tag:  tag,
	}
	return im.WriteEntry(e)
}

// WriteContext puts together the arguments to create an entry and writes it
// to the queue to be sent out by the first available
// entry writer routine, if all routines are dead, THIS WILL BLOCK once the
// channel fills up.  We figure this is a natural "wait" mechanism
// if the context isn't needed use Write instead
func (im *IngestMuxer) WriteContext(ctx context.Context, tm entry.Timestamp, tag entry.EntryTag, data []byte) error {
	e := &entry.Entry{
		Data: data,
		TS:   tm,
		Tag:  tag,
	}
	return im.WriteEntryContext(ctx, e)
}

//connFailed will put the destination in a failed state and inform the muxer
func (im *IngestMuxer) connFailed(dst string, err error) {
	im.mtx.Lock()
	defer im.mtx.Unlock()
	im.errDest = append(im.errDest, TargetError{
		Address: dst,
		Error:   err,
	})
	im.errChan <- err
}

type connSet struct {
	ig  *IngestConnection
	tt  *tagTrans
	dst string
	src net.IP
}

//keep attempting to get a new connection set that we can actually write to
func (im *IngestMuxer) getNewConnSet(csc chan connSet, connFailure chan bool, orig bool) (nc connSet, ok bool) {
	if !orig {
		//try to send, if we can't just roll on
		select {
		case connFailure <- true:
		default:
		}
	}
	for {
		if nc, ok = <-csc; !ok {
			return
		}
		//attempt to clear the emergency queue and throw at our new connection
		if !im.eq.clear(nc.ig, nc.tt) || nc.ig.Sync() != nil {
			//try to send, if we can't just roll on
			select {
			case connFailure <- true:
			default:
			}
			ok = false
			continue
		}
		//ok, we synced, pass things back
		if orig {
			im.Info("connected to %v", nc.dst)
		} else {
			im.Info("re-connected to %v", nc.dst)
		}
		break
	}
	return
}

func tickerInterval() time.Duration {
	//return a time between 750 and 1250 milliseconds
	return time.Duration(750+rand.Int63n(500)) * time.Millisecond
}

func (im *IngestMuxer) shouldSched() bool {
	//if pipelines are empty, schedule ourselves so that we can get a better distribution of entries
	return len(im.igst) > 1 && len(im.eChan) == 0 && len(im.bChan) == 0
}

func (im *IngestMuxer) writeRelayRoutine(csc chan connSet, connFailure chan bool) {
	tmr := time.NewTimer(tickerInterval())
	defer tmr.Stop()
	defer close(connFailure)

	//grab our first conn set
	var tnc connSet
	var nc connSet
	var ok bool
	var err error
	var ttag entry.EntryTag
	if nc, ok = im.getNewConnSet(csc, connFailure, true); !ok {
		return
	}

	eC := im.eChan
	bC := im.bChan

inputLoop:
	for {
		select {
		case _ = <-im.dieChan:
			nc.ig.Sync()
			nc.ig.Close()
			return
		case e, ok := <-eC:
			if !ok {
				eC = nil
				if bC == nil {
					return
				}
				continue
			}
			if e == nil {
				continue
			}
			ttag, ok = nc.tt.Translate(e.Tag)
			if !ok {
				// If the ingest muxer has no idea what this tag is, drop it and notify
				if name, ok := im.LookupTag(e.Tag); !ok {
					im.Error("Got entry tagged with completely unknown intermediate tag %v, dropping it", e.Tag)
					continue inputLoop
				} else {
					im.Info("Got entry tagged with tag %v (%v), need to renegotiate connection", name, e.Tag)
					// Could not translate, but it's a valid tag the muxer has seen before.
					// We need to push this to the equeue and reconnect
					// so we get the correct tag set.
					im.recycleEntries(e, nil, nc.tt, false)
					if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
						break inputLoop
					}
					continue inputLoop
				}
			}
			e.Tag = ttag

			if len(e.SRC) == 0 {
				e.SRC = nc.src
			}
			if err = nc.ig.WriteEntry(e); err != nil {
				im.recycleEntries(e, nil, nc.tt, true)
				if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
					break inputLoop
				}
			}
			//hack to get better distribution across connections in an muxer
			if im.shouldSched() {
				if !tmr.Stop() {
					<-tmr.C
				}
				if !im.eq.clear(nc.ig, nc.tt) || nc.ig.Sync() != nil {
					if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
						break inputLoop
					}
				}
				tmr.Reset(tickerInterval())
				runtime.Gosched()
			}
		case b, ok := <-bC:
			if !ok {
				bC = nil
				if eC == nil {
					return
				}
				continue
			}
			if b == nil {
				continue
			}
			for i := range b {
				if b[i] != nil {
					ttag, ok = nc.tt.Translate(b[i].Tag)
					if !ok {
						if name, ok := im.LookupTag(b[i].Tag); !ok {
							im.Error("Got entry tagged with completely unknown intermediate tag %v, dropping it", b[i].Tag)
							continue inputLoop
						} else {
							im.Info("Got entry tagged with tag %v (%v), need to renegotiate connection", name, b[i].Tag) // Could not translate! We need to push this to the equeue and reconnect
							// so we get the correct tag set.

							// first, reverse anything we've translated already
							for j := 0; j < i; j++ {
								b[j].Tag = nc.tt.Reverse(b[j].Tag)
							}
							im.recycleEntries(nil, b, nc.tt, false)
							if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
								break inputLoop
							}
							continue inputLoop
						}
					}
					b[i].Tag = ttag

					if len(b[i].SRC) == 0 {
						b[i].SRC = nc.src
					}
				}
			}
			if err = nc.ig.WriteBatchEntry(b); err != nil {
				im.recycleEntries(nil, b, nc.tt, true)
				if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
					break inputLoop
				}
			}
			//hack to get better distribution across connections in an muxer
			if im.shouldSched() {
				if !tmr.Stop() {
					<-tmr.C
				}
				if !im.eq.clear(nc.ig, nc.tt) || nc.ig.Sync() != nil {
					if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
						break inputLoop
					}
				}
				tmr.Reset(tickerInterval())
				runtime.Gosched()
			}
		case tnc, ok = <-csc: //in case we get an unexpected new connection
			if !ok {
				nc.ig.Sync()
				nc.ig.Close()
				//attempt to sync with current ngst and then bail
				break inputLoop
			}
			nc = tnc //just an update
		case <-tmr.C:
			//periodically check the emergency queue and sync
			if !im.eq.clear(nc.ig, nc.tt) || nc.ig.Sync() != nil {
				if nc, ok = im.getNewConnSet(csc, connFailure, false); !ok {
					break inputLoop
				}
			}
			tmr.Reset(tickerInterval())
		}
	}
}

//the routine that manages
func (im *IngestMuxer) connRoutine(igIdx int) {
	var src net.IP
	defer im.wg.Done()
	if igIdx >= len(im.igst) || igIdx >= len(im.dests) {
		//this SHOULD NEVER HAPPEN.  Bail
		im.connFailed(unknownAddr, errors.New("Invalid ingester index on muxer"))
		return
	}
	dst := im.dests[igIdx]
	if im.igst[igIdx] != nil {
		//this SHOULD NEVER HAPPEN.  Bail
		im.connFailed(dst.Address, errors.New("Ingester already populated for destination in muxer"))
		return
	}

	var igst *IngestConnection
	var tt tagTrans
	var err error
	connErrNotif := make(chan bool, 1)
	ncc := make(chan connSet, 1)
	defer close(ncc)

	go im.writeRelayRoutine(ncc, connErrNotif)

	connErrNotif <- true

	//loop, trying to grab entries, or dying
	for {
		select {
		case _, ok := <-connErrNotif:
			if igst != nil {
				//if it throws an error we don't care, and cant do anything about it
				im.Warn("reconnecting to %v", dst.Address)
				igst.Close()
			}
			if !ok {
				//this means that the relay function bailed
				im.goDead()
				im.connFailed(dst.Address, errors.New("Closed"))
				return
			}

			if igst != nil {
				im.goDead() //let the world know of our failures
				im.igst[igIdx] = nil
				im.tagTranslators[igIdx] = nil

				//pull any entries out of the ingest connection and put them into the emergency queue
				ents := igst.outstandingEntries()
				im.recycleEntries(nil, ents, &tt, true)
			}

			//attempt to get the connection rolling again
			igst, tt, err = im.getConnection(dst)
			if err != nil {
				im.connFailed(dst.Address, err)
				return //we are done
			}
			if igst == nil {
				im.connFailed(dst.Address, errors.New("Nil connection"))
				return
			}

			//get the source fired back up
			src, err = igst.Source()
			if err != nil {
				igst.Close()
				im.connFailed(dst.Address, err)
				return
			}

			im.mtx.Lock()
			im.igst[igIdx] = igst
			im.tagTranslators[igIdx] = &tt
			im.mtx.Unlock()

			im.goHot()
			ncc <- connSet{
				dst: dst.Address,
				src: src,
				ig:  igst,
				tt:  &tt,
			}
		}
	}
}

//we don't want to fully block here, so we attempt to push back on the channel
//and listen for a die signal
func (im *IngestMuxer) recycleEntries(e *entry.Entry, ents []*entry.Entry, tt *tagTrans, reverseTags bool) {
	//reset the tags to the globally translatable set
	//this operation is expensive
	if len(ents) > 0 && reverseTags {
		for i := range ents {
			if ents[i] != nil {
				ents[i].Tag = tt.Reverse(ents[i].Tag)
			}
		}
	}

	//we wait for up to one second to push values onto feeder channels
	//if nothing eats them by then, we drop them into the emergency queue
	//and bail out
	tmr := time.NewTimer(recycleTimeout)
	defer tmr.Stop()

	// try the single entry
	if e != nil {
		e.Tag = tt.Reverse(e.Tag)
		select {
		case _ = <-tmr.C:
			if err := im.eq.push(e, ents); err != nil {
				//FIXME - throw a fit about this via some logging, aight?
				return
			}
			//timer expired, reset it in case we have a block too
			tmr.Reset(0)
		case im.eChan <- e:
		}
	}
	//try block entry
	if len(ents) > 0 {
		select {
		case _ = <-tmr.C:
			if err := im.eq.push(nil, ents); err != nil {
				//FIXME - throw a fit about this
				return
			}
		case im.bChan <- ents:
		}
	}
	return
}

//fatal connection errors is looking for errors which are non-recoverable
//Recoverable errors are related to timeouts, refused connections, and read errors
func isFatalConnError(err error) bool {
	if err == nil {
		return false
	}
	switch err {
	case ErrMalformedDestination:
		fallthrough
	case ErrInvalidConnectionType:
		fallthrough
	case ErrFailedAuthHashGen:
		fallthrough
	case ErrForbiddenTag:
		fallthrough
	case ErrFailedParseLocalIP:
		fallthrough
	case ErrEmptyTag:
		return true
	}
	return false
}

func (im *IngestMuxer) getConnection(tgt Target) (ig *IngestConnection, tt tagTrans, err error) {
loop:
	for {
		//attempt a connection, timeouts are built in to the IngestConnection
		im.mtx.RLock()
		if ig, err = InitializeConnection(tgt.Address, tgt.Secret, im.tags, im.pubKey, im.privKey, im.verifyCert); err != nil {
			im.mtx.RUnlock()
			if isFatalConnError(err) {
				im.Error("Fatal Connection Error on %v: %v", tgt.Address, err)
				break loop
			}
			im.Warn("Connection error on %v: %v", tgt.Address, err)
			//non-fatal, sleep and continue
			select {
			case _ = <-time.After(defaultRetryTime):
			case _ = <-im.dieChan:
				//told to exit, just bail
				return nil, nil, errors.New("Muxer closing")
			}
			continue
		}
		if im.rateParent != nil {
			ig.ew.SetConn(im.rateParent.newThrottleConn(ig.ew.conn))
		}

		//no error, attempt to do a tag translation
		//we have a good connection, build our tag map
		if tt, err = im.newTagTrans(ig); err != nil {
			ig.Close()
			ig = nil
			tt = nil
			im.mtx.RUnlock()
			im.Error("Fatal Connection Error, failed to get get tag translation map: %v", err)
			continue
		}
		im.mtx.RUnlock()

		// set the info
		if err := ig.IdentifyIngester(im.name, im.version, im.uuid); err != nil {
			im.Error("Failed to identify ingester on %v: %v", tgt.Address, err)
			continue
		}

		for {
			select {
			case _ = <-im.dieChan:
				return
			default:
			}
			ok, err := ig.IngestOK()
			if err != nil {
				im.Error("IngestOK query failed on %v: %v", tgt.Address, err)
				continue loop
			}
			if ok {
				break
			}
			time.Sleep(5 * time.Second)
		}

		im.Info("Successfully connected to %v", tgt.Address)
		break
	}
	return
}

func (im *IngestMuxer) newTagTrans(igst *IngestConnection) (tagTrans, error) {
	tt := tagTrans(make([]entry.EntryTag, len(im.tagMap)))
	if len(tt) == 0 {
		return nil, ErrTagMapInvalid
	}
	for k, v := range im.tagMap {
		if int(v) > len(tt) {
			return nil, ErrTagMapInvalid
		}
		tg, ok := igst.GetTag(k)
		if !ok {
			return nil, ErrTagNotFound
		}
		tt[v] = tg
	}
	return tt, nil
}

// SourceIP is a convenience function used to pull back a source value
func (im *IngestMuxer) SourceIP() (net.IP, error) {
	var ip net.IP
	im.mtx.RLock()
	defer im.mtx.RUnlock()
	if im.connHot == 0 || len(im.igst) == 0 {
		return ip, errors.New("No active connections")
	}
	var set bool
	var wasErr bool
	for _, ig := range im.igst {
		if ig == nil {
			continue
		}
		lip, err := ig.Source()
		if err != nil {
			wasErr = true
			continue
		}
		if bytes.Compare(lip, localSrc) == 0 {
			continue
		}
		ip = lip
		set = true
	}
	if set {
		return ip, nil
	}
	if !wasErr {
		//this means there were no errors, we just have a local connection
		//this can happen
		return localSrc, nil
	}
	//just straight up couldn't get it
	return ip, errors.New("Failed to get remote connection")
}

type emStruct struct {
	e    *entry.Entry
	ents []*entry.Entry
}

type emergencyQueue struct {
	mtx *sync.Mutex
	lst *list.List
}

func newEmergencyQueue() *emergencyQueue {
	return &emergencyQueue{
		mtx: &sync.Mutex{},
		lst: list.New(),
	}
}

// emergencyPush is a last ditch effort to store
// items into a list of entries or blocks.  This should only be invoked when
// we are under very heavy load and have no indexer connections.  As a result
// the channels are all full and we can't recycle entries back into the feeders
// we this ingest connection disconnects.  Instead we push into this queue
// when new ingest connections become active, they will always attempt to feed from
// this queue before going to the channels.  This is essentially a deadlock fix.
func (eq *emergencyQueue) push(e *entry.Entry, ents []*entry.Entry) error {
	if e == nil && len(ents) == 0 {
		return nil
	}
	ems := emStruct{
		e:    e,
		ents: ents,
	}
	eq.mtx.Lock()
	if eq.lst.Len() > maxEmergencyListSize {
		eq.mtx.Unlock()
		return ErrEmergencyListOverflow
	}
	eq.lst.PushBack(ems)
	eq.mtx.Unlock()
	return nil
}

// emergencyPop checks to see if there are any values on the emergency list
// waiting to be ingested.  New routines should go to this list FIRST
func (eq *emergencyQueue) pop() (e *entry.Entry, ents []*entry.Entry, ok bool) {
	var elm emStruct
	eq.mtx.Lock()
	defer eq.mtx.Unlock()
	if eq.lst.Len() == 0 {
		//nothing here, bail
		return
	}
	el := eq.lst.Front()
	if el == nil {
		return
	}
	eq.lst.Remove(el) //its valid, remove it
	elm, ok = el.Value.(emStruct)
	if !ok {
		//shit?  FIXME - THROW A FIT
		return
	}
	e = elm.e
	ents = elm.ents
	return
}

func (eq *emergencyQueue) clear(igst *IngestConnection, tt *tagTrans) (ok bool) {
	//iterate on the emergency queue attempting to write elements to the remote side
	var ttag entry.EntryTag
	for {
		e, blk, populated := eq.pop()
		if !populated {
			ok = true
			break
		}
		if e != nil {
			ttag, ok = tt.Translate(e.Tag)
			if !ok {
				// could not translate, push it back on the queue and bail
				eq.push(e, blk)
				return
			}
			e.Tag = ttag
			if err := igst.WriteEntry(e); err != nil {
				//reset the tag
				e.Tag = tt.Reverse(e.Tag)

				//push the entries back into the queue
				if err := eq.push(e, blk); err != nil {
					//FIXME - log this?
				}

				//return our failure
				break
			}
			//all is good set e to nil in case we can't write the block
			e = nil
		}
		if len(blk) > 0 {
			//translate tags, SRC is always fixed up on pulling from the channel
			//so no need to check or set here
			for i := range blk {
				if blk[i] != nil {
					ttag, ok = tt.Translate(blk[i].Tag)
					if !ok {
						// could not translate, push it back on the queue and bail
						// first we need to reverse the ones we have already translated, ugh
						for j := 0; j < i; j++ {
							blk[j].Tag = tt.Reverse(blk[j].Tag)
						}
						eq.push(e, blk)
						return
					}
					blk[i].Tag = ttag
				}
			}
			if err := igst.WriteBatchEntry(blk); err != nil {
				//reverse the tags and push back into queue
				for i := range blk {
					if blk[i] != nil {
						blk[i].Tag = tt.Reverse(blk[i].Tag)
					}
				}
				if err := eq.push(e, blk); err != nil {
					//FIXME - log this?
				}
				break
			}
		}
	}
	return
}

type tagTrans []entry.EntryTag

// Translate translates a local tag to a remote tag.  Senders should not use this function
func (tt tagTrans) Translate(t entry.EntryTag) (entry.EntryTag, bool) {
	//check if this is the gravwell and if soo, pass it on through
	if t == entry.GravwellTagId {
		return t, true
	}
	//if this is a tag we have not negotiated, set it to the first one we have
	//we are assuming that its an error, but we still want the entry
	if int(t) >= len(tt) {
		return tt[0], false
	}
	return tt[t], true
}

func (tt *tagTrans) RegisterTag(local entry.EntryTag, remote entry.EntryTag) error {
	if int(local) != len(*tt) {
		// this means the local tag numbers got out of sync and something is bad
		return errors.New("Cannot register tag, local tag out of sync with tag translator")
	}
	*tt = append(*tt, remote)
	return nil
}

// Reverse translates a remote tag back to a local tag
// this is ONLY used when a connection dies while holding unconfirmed entries
// this operation is stupid expensive, so... be gracious
func (tt tagTrans) Reverse(t entry.EntryTag) entry.EntryTag {
	//check if this is gravwell and if soo, pass it on through
	if t == entry.GravwellTagId {
		return t
	}
	for i := range tt {
		if tt[i] == t {
			return entry.EntryTag(i)
		}
	}
	return 0
}
