package couchbase

import (
	"log"
	"sync"
	"time"

	"github.com/dustin/gomemcached/client"
)

const initialRetryInterval = 1 * time.Second
const maximumRetryInterval = 30 * time.Second

// A TapFeed streams mutation events from a bucket.
//
// Events from the bucket can be read from the channel 'C'.  Remember
// to call Close() on it when you're done, unless its channel has
// closed itself already.
type TapFeed struct {
	C <-chan memcached.TapEvent

	bucket    *Bucket
	args      *memcached.TapArguments
	nodeFeeds []*memcached.TapFeed    // The TAP feeds of the individual nodes
	output    chan memcached.TapEvent // Same as C but writeably-typed
	quit      chan bool
}

// StartTapFeed creates and starts a new Tap feed
func (b *Bucket) StartTapFeed(args *memcached.TapArguments) (*TapFeed, error) {
	if args == nil {
		defaultArgs := memcached.DefaultTapArguments()
		args = &defaultArgs
	}

	feed := &TapFeed{
		bucket: b,
		args:   args,
		output: make(chan memcached.TapEvent, 10),
		quit:   make(chan bool),
	}

	go feed.run()

	feed.C = feed.output
	return feed, nil
}

// Goroutine that runs the feed
func (feed *TapFeed) run() {
	retryInterval := initialRetryInterval
	bucketOK := true
	for {
		// Connect to the TAP feed of each server node:
		var feedErr error
		if bucketOK {
			killSwitch, err := feed.connectToNodes()
			if err == nil {
				// Run until one of the sub-feeds fails:
				select {
				case feedErr = <-killSwitch:
					if feedErr == nil {
						feed.Close()
						return
					}
				case <-feed.quit:
					return
				}
				feed.closeNodeFeeds()
				retryInterval = initialRetryInterval
			}
		}

		// On error, try to refresh the bucket in case the list of nodes changed:
		log.Printf("go-couchbase: TAP connection lost %v; reconnecting to bucket %q in %v",
			feedErr, feed.bucket.Name, retryInterval)
		err := feed.bucket.refresh()
		bucketOK = err == nil
		if !bucketOK {
			log.Printf("go-couchbase: refresh of bucket %v failed: %v",
				feed.bucket.Name, err)
		}

		select {
		case <-time.After(retryInterval):
		case <-feed.quit:
			return
		}
		if retryInterval *= 2; retryInterval > maximumRetryInterval {
			retryInterval = maximumRetryInterval
		}
	}
}

func (feed *TapFeed) connectToNodes() (killSwitch chan error, err error) {
	var wg sync.WaitGroup
	pools := feed.bucket.getConnPools()
	killSwitch = make(chan error, len(pools))
	for _, serverConn := range pools {
		var singleFeed *memcached.TapFeed
		singleFeed, err = serverConn.StartTapFeed(feed.args)
		if err != nil {
			log.Printf("go-couchbase: Error connecting to tap feed of %s: %v", serverConn.host, err)
			feed.closeNodeFeeds()
			return
		}
		feed.nodeFeeds = append(feed.nodeFeeds, singleFeed)
		wg.Add(1)
		go feed.forwardTapEvents(singleFeed, killSwitch, serverConn.host, &wg)
	}

	go func() {
		wg.Wait()
		close(feed.output)
	}()
	return
}

// Goroutine that forwards Tap events from a single node's feed to the aggregate feed.
func (feed *TapFeed) forwardTapEvents(singleFeed *memcached.TapFeed, killSwitch chan error, host string, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case event, ok := <-singleFeed.C:
			if !ok {
				if singleFeed.Error != nil {
					log.Printf("go-couchbase: Tap feed from %s failed: %v", host, singleFeed.Error)
				}
				killSwitch <- singleFeed.Error
				return
			}
			feed.output <- event
		case <-feed.quit:
			return
		}
	}
}

func (feed *TapFeed) closeNodeFeeds() {
	for _, f := range feed.nodeFeeds {
		f.Close()
	}
	feed.nodeFeeds = nil
}

// Close a Tap feed.
func (feed *TapFeed) Close() error {
	select {
	case <-feed.quit:
		return nil
	default:
	}

	feed.closeNodeFeeds()
	close(feed.quit)
	return nil
}
