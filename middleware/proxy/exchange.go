package proxy

import "github.com/miekg/dns"

type Exchanger interface {
	Exchange(dns.ResponseWriter, *dns.Msg, string) (*dns.Msg, error)
}
