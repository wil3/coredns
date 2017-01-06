package proxy

import "github.com/miekg/dns"

type Exchanger interface {
	Exchange(dns.ResponseWriter, *dns.Msg, *UpstreamHost) (*dns.Msg, error)
}
