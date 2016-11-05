// Package etcd3 provides the etcd3 backend middleware.
package etcd3

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/miekg/coredns/middleware"
	"github.com/miekg/coredns/middleware/etcd/msg"
	"github.com/miekg/coredns/middleware/pkg/singleflight"
	"github.com/miekg/coredns/middleware/proxy"
	"github.com/miekg/coredns/request"

	etcdv3 "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

// Etcd3 is a middleware talks to an etcd cluster.
type Etcd3 struct {
	Next       middleware.Handler
	Zones      []string
	PathPrefix string
	Proxy      proxy.Proxy // Proxy for looking up names during the resolution process
	Client     etcdv3.Client
	Ctx        context.Context
	Inflight   *singleflight.Group
	Stubmap    *map[string]proxy.Proxy // list of proxies for stub resolving.
	Debugging  bool                    // Do we allow debug queries.

	endpoints []string // Stored here as well, to aid in testing.
}

// Services implements the ServiceBackend interface.
func (e *Etcd3) Services(state request.Request, exact bool, opt middleware.Options) (services, debug []msg.Service, err error) {
	services, err = e.Records(state.Name(), exact)
	if err != nil {
		return
	}
	if opt.Debug != "" {
		debug = services
	}
	services = msg.Group(services)
	return
}

// Lookup implements the ServiceBackend interface.
func (e *Etcd3) Lookup(state request.Request, name string, typ uint16) (*dns.Msg, error) {
	return e.Proxy.Lookup(state, name, typ)
}

// IsNameError implements the ServiceBackend interface.
func (e *Etcd3) IsNameError(err error) bool {
	//	if ee, ok := err.(etcdc.Error); ok && ee.Code == etcdc.ErrorCodeKeyNotFound {
	//		return true
	//	}
	return false
}

// Debug implements the ServiceBackend interface.
func (e *Etcd3) Debug() string {
	return e.PathPrefix
}

func (e *Etcd3) Records(name string, exact bool) ([]msg.Service, error) {
	path, star := msg.PathWithWildcard(name, e.PathPrefix)
	r, err := e.get(path, true)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(msg.Path(name, e.PathPrefix), "/")

	return e.loopNodes(r.Kvs, segments, star, nil)
}

// get is a wrapper for client.Get that uses SingleInflight to suppress multiple outstanding queries.
func (e *Etcd3) get(path string, recursive bool) (*etcdv3.GetResponse, error) {
	resp, err := e.Inflight.Do(path, func() (interface{}, error) {
		ctx, cancel := context.WithTimeout(e.Ctx, etcdTimeout)
		defer cancel()
		if recursive {
			r, e1 := e.Client.Get(ctx, path, etcdv3.WithPrefix())
			return r, e1
		}
		r, e1 := e.Client.Get(ctx, path)
		return r, e1

	})
	if err != nil {
		return nil, err
	}
	return resp.(*etcdv3.GetResponse), err
}

func (e *Etcd3) loopNodes(kv []*mvccpb.KeyValue, nameParts []string, star bool, bx map[msg.Service]bool) (sx []msg.Service, err error) {
	if bx == nil {
		bx = make(map[msg.Service]bool)
	}
Nodes:
	for _, item := range kv {

		if star {
			s := string(item.Key)
			keyParts := strings.Split(s, "/")
			for i, n := range nameParts {
				if i > len(keyParts)-1 {
					continue Nodes
				}
				if n == "*" || n == "any" {
					continue
				}
				if keyParts[i] != n {
					continue Nodes
				}
			}
		}

		serv := new(msg.Service)
		if err := json.Unmarshal(item.Value, serv); err != nil {
			return nil, err
		}

		b := msg.Service{Host: serv.Host, Port: serv.Port, Priority: serv.Priority, Weight: serv.Weight, Text: serv.Text, Key: string(item.Key)}
		if _, ok := bx[b]; ok {
			continue
		}
		bx[b] = true

		serv.Key = string(item.Key)
		//TODO: another call (LeaseRequest) for TTL when RPC in etcdv3 is ready
		serv.TTL = e.TTL(item, serv)
		if serv.Priority == 0 {
			serv.Priority = priority
		}

		sx = append(sx, *serv)
	}
	return sx, nil
}

func (e *Etcd3) TTL(kv *mvccpb.KeyValue, serv *msg.Service) uint32 {
	etcdTTL := uint32(kv.Lease) //TODO: still waiting for Least request rpc to be available in etcdv3's api

	if etcdTTL == 0 && serv.TTL == 0 {
		return ttl
	}
	if etcdTTL == 0 {
		return serv.TTL
	}
	if serv.TTL == 0 {
		return etcdTTL
	}
	if etcdTTL < serv.TTL {
		return etcdTTL
	}
	return serv.TTL
}

const (
	priority    = 10  // default priority when nothing is set
	ttl         = 300 // default ttl when nothing is set
	etcdTimeout = 5 * time.Second
)
