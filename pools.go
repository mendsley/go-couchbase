package couchbase

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// The HTTP Client To Use
var HttpClient = http.DefaultClient

// Auth callback gets the auth username and password for the given
// bucket.
type AuthHandler interface {
	GetCredentials() (string, string)
}

type RestPool struct {
	Name         string `json:"name"`
	StreamingURI string `json:"streamingUri"`
	URI          string `json:"uri"`
}

type Pools struct {
	ComponentsVersion     map[string]string `json:"componentsVersion,omitempty"`
	ImplementationVersion string            `json:"implementationVersion"`
	IsAdmin               bool              `json:"isAdminCreds"`
	UUID                  string            `json:"uuid"`
	Pools                 []RestPool        `json:"pools"`
}

// A computer in a cluster running the couchbase software.
type Node struct {
	ClusterCompatibility int                `json:"clusterCompatibility"`
	ClusterMembership    string             `json:"clusterMembership"`
	CouchAPIBase         string             `json:"couchApiBase"`
	Hostname             string             `json:"hostname"`
	InterestingStats     map[string]float64 `json:"interestingStats,omitempty"`
	MCDMemoryAllocated   float64            `json:"mcdMemoryAllocated"`
	MCDMemoryReserved    float64            `json:"mcdMemoryReserved"`
	MemoryFree           float64            `json:"memoryFree"`
	MemoryTotal          float64            `json:"memoryTotal"`
	OS                   string             `json:"os"`
	Ports                map[string]int     `json:"ports"`
	Status               string             `json:"status"`
	Uptime               int                `json:"uptime,string"`
	Version              string             `json:"version"`
	ThisNode             bool               `json:"thisNode,omitempty"`
}

// A pool of nodes and buckets.
type Pool struct {
	BucketMap map[string]Bucket
	Nodes     []Node

	BucketURL map[string]string `json:"buckets"`

	client Client
}

// An individual bucket.  Herein lives the most useful stuff.
type Bucket struct {
	AuthType            string             `json:"authType"`
	Capabilities        []string           `json:"bucketCapabilities"`
	CapabilitiesVersion string             `json:"bucketCapabilitiesVer"`
	Type                string             `json:"bucketType"`
	Name                string             `json:"name"`
	NodeLocator         string             `json:"nodeLocator"`
	Nodes               []Node             `json:"nodes"`
	Quota               map[string]float64 `json:"quota,omitempty"`
	Replicas            int                `json:"replicaNumber"`
	Password            string             `json:"saslPassword"`
	URI                 string             `json:"uri"`
	StreamingURI        string             `json:"streamingUri"`
	LocalRandomKeyURI   string             `json:"localRandomKeyUri,omitempty"`
	UUID                string             `json:"uuid"`
	DDocs               struct {
		URI string `json:"uri"`
	} `json:"ddocs,omitempty"`
	VBucketServerMap struct {
		HashAlgorithm string   `json:"hashAlgorithm"`
		NumReplicas   int      `json:"numReplicas"`
		ServerList    []string `json:"serverList"`
		VBucketMap    [][]int  `json:"vBucketMap"`
	} `json:"vBucketServerMap"`
	BasicStats  map[string]interface{} `json:"basicStats,omitempty"`
	Controllers map[string]interface{} `json:"controllers,omitempty"`

	pool        *Pool
	connections []*connectionPool
	commonSufix string
	lock        sync.RWMutex
}

func (b Bucket) authHandler() (ah AuthHandler) {
	if b.pool != nil {
		ah = b.pool.client.ah
	}
	if ah == nil {
		ah = &basicAuth{b.Name, ""}
	}
	return
}

// Get the (sorted) list of memcached node addresses (hostname:port).
func (b Bucket) NodeAddresses() []string {
	rv := make([]string, len(b.VBucketServerMap.ServerList))
	copy(rv, b.VBucketServerMap.ServerList)
	sort.Strings(rv)
	return rv
}

// Get the longest common suffix of all host:port strings in the node list.
func (b Bucket) CommonAddressSuffix() string {
	input := []string{}
	for _, n := range b.Nodes {
		input = append(input, n.Hostname)
	}
	return FindCommonSuffix(input)
}

// The couchbase client gives access to all the things.
type Client struct {
	BaseURL  *url.URL
	ah       AuthHandler
	Info     Pools
	Statuses [256]uint64
}

func maybeAddAuth(req *http.Request, ah AuthHandler) {
	if ah != nil {
		user, pass := ah.GetCredentials()
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
}

func (c *Client) parseURLResponse(path string, out interface{}) error {
	u := *c.BaseURL
	u.User = nil
	if q := strings.Index(path, "?"); q > 0 {
		u.Path = path[:q]
		u.RawQuery = path[q+1:]
	} else {
		u.Path = path
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}
	maybeAddAuth(req, c.ah)

	res, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		bod, _ := ioutil.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("HTTP error %v getting %q: %s",
			res.Status, u.String(), bod)
	}

	d := json.NewDecoder(res.Body)
	if err = d.Decode(&out); err != nil {
		return err
	}
	return nil
}

type basicAuth struct {
	u, p string
}

func (b basicAuth) GetCredentials() (string, string) {
	return b.u, b.p
}

func basicAuthFromURL(us string) (ah AuthHandler) {
	u, err := url.Parse(us)
	if err != nil {
		return
	}
	if user := u.User; user != nil {
		pw, _ := user.Password()
		ah = basicAuth{user.Username(), pw}
	}
	return
}

// ConnectWithAuth connects to a couchbase cluster with the given
// authentication handler.
func ConnectWithAuth(baseU string, ah AuthHandler) (c Client, err error) {
	c.BaseURL, err = url.Parse(baseU)
	if err != nil {
		return
	}
	c.ah = ah

	return c, c.parseURLResponse("/pools", &c.Info)
}

// Connect to a couchbase cluster.  An authentication handler will be
// created from the userinfo in the URL if provided.
func Connect(baseU string) (Client, error) {
	return ConnectWithAuth(baseU, basicAuthFromURL(baseU))
}

func (b *Bucket) getConnectionPool(key string) (*connectionPool, uint16) {
	b.lock.RLock()
	defer b.lock.RUnlock()
	vb := b.VBHash(key)
	if uint32(len(b.VBucketServerMap.VBucketMap)) < vb || len(b.VBucketServerMap.VBucketMap[vb]) < 1 {
		return nil, uint16(vb)
	}
	masterId := b.VBucketServerMap.VBucketMap[vb][0]
	return b.connections[masterId], uint16(vb)
}

func (b *Bucket) refresh() (err error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	pool := b.pool
	err = pool.client.parseURLResponse(b.URI, b)
	if err != nil {
		return err
	}
	b.pool = pool

	// build map of desired connections
	conns := make(map[string]*connectionPool)
	for _, host := range b.VBucketServerMap.ServerList {
		conns[host] = nil
	}

	// preserve existing connections, and close departing connections
	for _, cp := range b.connections {
		if _, ok := conns[cp.host]; ok {
			conns[cp.host] = cp
		} else {
			cp.Close()
		}
	}

	// craete new connection pools
	for host, cp := range conns {
		if cp == nil {
			conns[host] = newConnectionPool(
				host,
				b.authHandler(), 4)
		}
	}

	// rebuild connection list
	if cap(b.connections) < len(b.VBucketServerMap.ServerList) {
		b.connections = make([]*connectionPool, len(b.VBucketServerMap.ServerList))
	}
	b.connections = b.connections[:len(b.VBucketServerMap.ServerList)]
	for ii, host := range b.VBucketServerMap.ServerList {
		b.connections[ii] = conns[host]
	}

	return nil
}

func (p *Pool) refresh() (err error) {
	p.BucketMap = make(map[string]Bucket)

	buckets := []Bucket{}
	err = p.client.parseURLResponse(p.BucketURL["uri"], &buckets)
	if err != nil {
		return err
	}
	for _, b := range buckets {
		b.pool = p
		p.BucketMap[b.Name] = b
	}
	return nil
}

// Get a pool from within the couchbase cluster (usually "default").
func (c *Client) GetPool(name string) (p Pool, err error) {
	var poolURI string
	for _, p := range c.Info.Pools {
		if p.Name == name {
			poolURI = p.URI
		}
	}
	if poolURI == "" {
		return p, errors.New("No pool named " + name)
	}

	err = c.parseURLResponse(poolURI, &p)

	p.client = *c

	err = p.refresh()
	return
}

// Mark this bucket as no longer needed, closing connections it may have open.
func (b *Bucket) Close() {
	if b.connections != nil {
		for _, c := range b.connections {
			if c != nil {
				c.Close()
			}
		}
		b.connections = nil
	}
}

func bucket_finalizer(b *Bucket) {
	if b.connections != nil {
		log.Printf("Warning: Finalizing a bucket with active connections.")
	}
}

// Get a bucket from within this pool.
func (p *Pool) GetBucket(name string) (*Bucket, error) {
	rv, ok := p.BucketMap[name]
	if !ok {
		return nil, errors.New("No bucket named " + name)
	}
	runtime.SetFinalizer(&rv, bucket_finalizer)
	err := rv.refresh()
	if err != nil {
		return nil, err
	}
	return &rv, nil
}

// Get the pool to which this bucket belongs.
func (b *Bucket) GetPool() *Pool {
	return b.pool
}

// Get the client from which we got this pool.
func (p *Pool) GetClient() *Client {
	return &p.client
}

// Convenience function for getting a named bucket from a URL
func GetBucket(endpoint, poolname, bucketname string) (*Bucket, error) {
	var err error
	client, err := Connect(endpoint)
	if err != nil {
		return nil, err
	}

	pool, err := client.GetPool(poolname)
	if err != nil {
		return nil, err
	}

	return pool.GetBucket(bucketname)
}
